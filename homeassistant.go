package main

import (
	"encoding/json"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Home Assistant, over MQTT discovery: give the bridge a broker (the
// Mosquitto add-on, usually) and a "Kerf Bridge" device appears in Home
// Assistant by itself — the laser's state, the job and its progress, the
// head's position, a picture of the bed while a job runs, and Stop, Pause
// and Resume buttons. Stop always works, so an automation can stop the
// laser (a smoke alarm going off, say). Nothing to install in Home
// Assistant.
//
// Topics, for a bridge named "Kerf Bridge (pibuild)":
//
//	kerf/kerf_bridge_pibuild/status    online / offline (retained, will)
//	kerf/kerf_bridge_pibuild/state     JSON, on change and every 15 s
//	kerf/kerf_bridge_pibuild/command   stop | pause | resume
//	kerf/kerf_bridge_pibuild/camera    JPEG, every 5 s while a job runs
//	homeassistant/<component>/kerf_bridge_pibuild/<object>/config  (retained)
type HomeAssistant struct {
	m   *Machine
	cam *Camera
	cfg func() Config

	mu     sync.Mutex
	status HAStatus
}

// HAStatus is how the link to Home Assistant is doing, for the web page.
type HAStatus struct {
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Broker    string `json:"broker,omitempty"`
	Error     string `json:"error,omitempty"`
}

// haSettings are the parts of the config that, when changed, need a new
// connection.
type haSettings struct {
	broker, user, pass, prefix, name, page string
	camera                                 bool
}

func haSettingsOf(c Config) haSettings {
	prefix := strings.Trim(c.MQTTPrefix, "/ ")
	if prefix == "" {
		prefix = "homeassistant"
	}
	page := ""
	if host := announceHost(); host != "" {
		port := "8023"
		if _, p, err := net.SplitHostPort(c.HTTPAddr); err == nil && p != "" {
			port = p
		}
		page = "http://" + net.JoinHostPort(host, port)
	}
	return haSettings{broker: strings.TrimSpace(c.MQTTBroker), user: c.MQTTUsername, pass: c.MQTTPassword,
		prefix: prefix, name: c.Name, page: page, camera: c.MQTTCamera}
}

// haStatePeriod: the state is sent at least this often, changed or not.
const haStatePeriod = 15 * time.Second

// haCameraPeriod: a picture of the bed this often while a job runs.
const haCameraPeriod = 5 * time.Second

// haRetry: after a failed connection, how long before trying again.
var haRetry = 15 * time.Second

func StartHomeAssistant(m *Machine, cam *Camera, cfg func() Config, stop chan struct{}) *HomeAssistant {
	ha := &HomeAssistant{m: m, cam: cam, cfg: cfg}
	go ha.run(stop)
	return ha
}

func (ha *HomeAssistant) Status() HAStatus {
	ha.mu.Lock()
	defer ha.mu.Unlock()
	return ha.status
}

func (ha *HomeAssistant) setStatus(f func(*HAStatus)) {
	ha.mu.Lock()
	f(&ha.status)
	ha.mu.Unlock()
}

func (ha *HomeAssistant) run(stop chan struct{}) {
	for {
		s := haSettingsOf(ha.cfg())
		if s.broker == "" {
			ha.setStatus(func(st *HAStatus) { *st = HAStatus{} })
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if !ha.session(s, stop) {
			return
		}
	}
}

// nodeID: "Kerf Bridge (pibuild)" → "kerf_bridge_pibuild".
func nodeID(name string) string {
	var b strings.Builder
	under := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			under = false
		} else if !under && b.Len() > 0 {
			b.WriteByte('_')
			under = true
		}
	}
	id := strings.Trim(b.String(), "_")
	if !strings.HasPrefix(id, "kerf") {
		id = "kerf_" + id
	}
	return strings.TrimSuffix(id, "_")
}

// brokerURL: "192.168.0.176" → "tcp://192.168.0.176:1883".
func brokerURL(b string) string {
	if !strings.Contains(b, "://") {
		b = "tcp://" + b
	}
	scheme, rest, _ := strings.Cut(b, "://")
	if _, _, err := net.SplitHostPort(rest); err != nil {
		port := "1883"
		if scheme == "ssl" || scheme == "tls" || scheme == "mqtts" {
			port = "8883"
		}
		rest = net.JoinHostPort(strings.Trim(rest, "[]"), port)
	}
	return scheme + "://" + rest
}

// session connects and keeps Home Assistant up to date until the settings
// change (returns true) or the bridge stops (false).
func (ha *HomeAssistant) session(s haSettings, stop chan struct{}) bool {
	node := nodeID(s.name)
	base := "kerf/" + node
	url := brokerURL(s.broker)
	ha.setStatus(func(st *HAStatus) { *st = HAStatus{Enabled: true, Broker: url} })

	opts := mqtt.NewClientOptions().
		AddBroker(url).
		SetClientID(node).
		SetUsername(s.user).
		SetPassword(s.pass).
		SetKeepAlive(30*time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30*time.Second).
		SetWill(base+"/status", "offline", 1, true)
	sendNow := make(chan struct{}, 1)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		ha.announce(c, s, node, base)
		c.Publish(base+"/status", 1, true, "online")
		c.Subscribe(base+"/command", 1, func(_ mqtt.Client, msg mqtt.Message) {
			ha.command(strings.ToLower(strings.TrimSpace(string(msg.Payload()))))
		})
		ha.setStatus(func(st *HAStatus) { st.Connected, st.Error = true, "" })
		log.Printf("home assistant: connected to %s as %s", url, node)
		select {
		case sendNow <- struct{}{}:
		default:
		}
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		ha.setStatus(func(st *HAStatus) { st.Connected, st.Error = false, "lost the broker: "+err.Error() })
		log.Printf("home assistant: lost the broker: %v", err)
	})
	client := mqtt.NewClient(opts)

	// The first connection by hand, so a wrong address or login shows on
	// the web page; after that the client reconnects by itself.
	for {
		tok := client.Connect()
		if !tok.WaitTimeout(15 * time.Second) {
			ha.setStatus(func(st *HAStatus) { st.Error = "the broker didn't answer" })
		} else if err := tok.Error(); err != nil {
			ha.setStatus(func(st *HAStatus) { st.Error = describeMQTT(err) })
		} else {
			break
		}
		log.Printf("home assistant: can't connect to %s: %s", url, ha.Status().Error)
		select {
		case <-stop:
			return false
		case <-time.After(haRetry):
		}
		if haSettingsOf(ha.cfg()) != s {
			return true
		}
	}
	defer func() {
		client.Publish(base+"/status", 1, true, "offline").WaitTimeout(2 * time.Second)
		client.Disconnect(500)
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last string
	var lastSent, lastPicture time.Time
	wasActive := false
	for {
		select {
		case <-stop:
			return false
		case <-tick.C:
		case <-sendNow:
			last = ""
		}
		if haSettingsOf(ha.cfg()) != s {
			ha.clearRemoved(client, s, haSettingsOf(ha.cfg()), node)
			return true
		}
		if !client.IsConnected() {
			continue
		}
		snap := ha.m.Snapshot()
		state := haState(snap, time.Now())
		if state != last || time.Since(lastSent) >= haStatePeriod {
			client.Publish(base+"/state", 0, false, state)
			last, lastSent = state, time.Now()
		}
		active := snap.Job != nil && snap.Job.State.active()
		if s.camera && (active && time.Since(lastPicture) >= haCameraPeriod || wasActive && !active) {
			lastPicture = time.Now()
			go ha.sendPicture(client, base)
		}
		wasActive = active
	}
}

func (ha *HomeAssistant) sendPicture(c mqtt.Client, base string) {
	jpeg, _, err := ha.cam.Frame(0, 4*time.Second)
	if err == nil {
		c.Publish(base+"/camera", 0, false, jpeg)
	}
}

func (ha *HomeAssistant) command(cmd string) {
	switch cmd {
	case "stop":
		log.Printf("home assistant: stop")
		go ha.m.Stop()
	case "pause":
		log.Printf("home assistant: pause")
		ha.m.Pause()
	case "resume":
		log.Printf("home assistant: resume")
		if err := ha.m.Resume(); err != nil {
			log.Printf("home assistant: resume: %v", err)
		}
	default:
		log.Printf("home assistant: unknown command %q", cmd)
	}
}

// haState is the state message: everything the sensors read, in one JSON
// object.
func haState(s Snapshot, now time.Time) string {
	out := map[string]any{
		"state":     laserLabel(s),
		"connected": onOff(s.Connected),
		"busy":      onOff(s.Job != nil && s.Job.State == JobRunning),
		"job":       "None",
		"job_state": "None",
		"progress":  0.0,
		"time_left": nil,
		"x":         nil,
		"y":         nil,
	}
	if s.Status != nil && s.Connected {
		out["x"] = math.Round(s.Status.WPos[0]*10) / 10
		out["y"] = math.Round(s.Status.WPos[1]*10) / 10
	}
	if j := s.Job; j != nil {
		out["job"] = j.Name
		out["job_state"] = jobLabel(j.State)
		if j.Total > 0 {
			f := float64(j.Done) / float64(j.Total)
			out["progress"] = math.Round(f*1000) / 10
			// Time left from the pace so far; a rough guide (lines aren't
			// equal), and only once there's enough to go on.
			if j.State == JobRunning && f > 0.02 && f < 1 {
				elapsed := now.Sub(j.StartedAt).Minutes()
				out["time_left"] = math.Round(elapsed * (1 - f) / f)
			}
		}
	}
	data, _ := json.Marshal(out)
	return string(data)
}

func laserLabel(s Snapshot) string {
	if !s.Connected {
		return "Not connected"
	}
	if s.Job != nil && (s.Job.State == JobPaused || s.Job.State == JobPausedOnError) {
		return "Paused"
	}
	state := ""
	if s.Status != nil {
		state = s.Status.State
	}
	switch {
	case state == "Idle":
		return "Idle"
	case state == "Run":
		if s.Job != nil && s.Job.State == JobRunning {
			return "Burning"
		}
		return "Running"
	case strings.HasPrefix(state, "Hold"):
		return "Held"
	case strings.HasPrefix(state, "Door"):
		return "Lid open"
	case strings.HasPrefix(state, "Alarm"):
		return "Alarm"
	case state == "Jog":
		return "Moving"
	case state == "Home":
		return "Homing"
	case state == "":
		return "Connected"
	}
	return state
}

func jobLabel(s JobState) string {
	switch s {
	case JobRunning:
		return "Running"
	case JobPaused:
		return "Paused"
	case JobPausedOnError:
		return "Held after an error"
	case JobCompleted:
		return "Finished"
	case JobStopped:
		return "Stopped"
	case JobAlarmed:
		return "Ended by an alarm"
	case JobInterrupted:
		return "Interrupted"
	}
	return string(s)
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// haEntity is one discovery message.
type haEntity struct {
	component, object string
	config            map[string]any
}

func haEntities(s haSettings, node, base string) []haEntity {
	device := map[string]any{
		"identifiers":  []string{node},
		"name":         s.name,
		"manufacturer": "Kerf",
		"model":        "Kerf Bridge",
		"sw_version":   Version,
	}
	if s.page != "" {
		device["configuration_url"] = s.page
	}
	common := func(object, name, icon string) map[string]any {
		c := map[string]any{
			"name":               name,
			"unique_id":          node + "_" + object,
			"object_id":          node + "_" + object,
			"device":             device,
			"availability_topic": base + "/status",
		}
		if icon != "" {
			c["icon"] = icon
		}
		return c
	}
	sensor := func(object, name, icon, field string, extra map[string]any) haEntity {
		c := common(object, name, icon)
		c["state_topic"] = base + "/state"
		c["value_template"] = "{{ value_json." + field + " }}"
		for k, v := range extra {
			c[k] = v
		}
		return haEntity{"sensor", object, c}
	}
	binary := func(object, name, class, field string, extra map[string]any) haEntity {
		c := common(object, name, "")
		c["state_topic"] = base + "/state"
		c["value_template"] = "{{ value_json." + field + " }}"
		c["device_class"] = class
		for k, v := range extra {
			c[k] = v
		}
		return haEntity{"binary_sensor", object, c}
	}
	button := func(object, name, icon, payload string) haEntity {
		c := common(object, name, icon)
		c["command_topic"] = base + "/command"
		c["payload_press"] = payload
		return haEntity{"button", object, c}
	}
	list := []haEntity{
		sensor("laser", "Laser", "mdi:laser-pointer", "state", nil),
		sensor("job", "Job", "mdi:file-document-outline", "job", nil),
		sensor("job_state", "Job state", "mdi:progress-check", "job_state", nil),
		sensor("progress", "Job progress", "mdi:progress-clock", "progress",
			map[string]any{"unit_of_measurement": "%", "state_class": "measurement", "suggested_display_precision": 1}),
		sensor("time_left", "Time left", "mdi:timer-sand", "time_left",
			map[string]any{"unit_of_measurement": "min", "device_class": "duration"}),
		sensor("x", "Head X", "mdi:axis-x-arrow", "x",
			map[string]any{"unit_of_measurement": "mm", "suggested_display_precision": 1, "entity_category": "diagnostic"}),
		sensor("y", "Head Y", "mdi:axis-y-arrow", "y",
			map[string]any{"unit_of_measurement": "mm", "suggested_display_precision": 1, "entity_category": "diagnostic"}),
		binary("connected", "Laser connected", "connectivity", "connected",
			map[string]any{"entity_category": "diagnostic"}),
		binary("burning", "Burning", "running", "busy", nil),
		button("stop", "Stop", "mdi:stop-circle", "stop"),
		button("pause", "Pause", "mdi:pause-circle", "pause"),
		button("resume", "Resume", "mdi:play-circle", "resume"),
	}
	if s.camera {
		c := common("camera", "Bed camera", "mdi:camera")
		c["topic"] = base + "/camera"
		list = append(list, haEntity{"camera", "camera", c})
	}
	return list
}

func (ha *HomeAssistant) announce(c mqtt.Client, s haSettings, node, base string) {
	for _, e := range haEntities(s, node, base) {
		data, _ := json.Marshal(e.config)
		c.Publish(s.prefix+"/"+e.component+"/"+node+"/"+e.object+"/config", 1, true, data)
	}
}

// clearRemoved takes entities off Home Assistant that the new settings drop
// (the camera turned off, or a new prefix or name: the old ones go).
func (ha *HomeAssistant) clearRemoved(c mqtt.Client, old, now haSettings, node string) {
	if !c.IsConnected() {
		return
	}
	keep := map[string]bool{}
	if old.prefix == now.prefix && nodeID(now.name) == node {
		for _, e := range haEntities(now, node, "") {
			keep[e.component+"/"+e.object] = true
		}
	}
	for _, e := range haEntities(old, node, "") {
		if !keep[e.component+"/"+e.object] {
			c.Publish(old.prefix+"/"+e.component+"/"+node+"/"+e.object+"/config", 1, true, "").WaitTimeout(time.Second)
		}
	}
}

func describeMQTT(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not Authorized"), strings.Contains(msg, "bad user name or password"):
		return "the broker refused the username or password"
	case strings.Contains(msg, "connection refused"):
		return "nothing answered at that address — check the broker's address and port"
	case strings.Contains(msg, "no such host"):
		return "that broker address can't be found"
	}
	return msg
}
