#!/bin/bash
# Makes Kerf Bridge.app from a built (universal) bridge program, signs it
# with the Developer ID certificate, notarises it with Apple using the Apple
# account signed in to Xcode (Xcode > Settings > Accounts; no app-specific
# password or API key), staples the ticket, and wraps it in a DMG.
#
#   bridge/macos/make-app.sh <kerf-bridge program> <version> <out.dmg>
#   SIGN_ONLY=1 bridge/macos/make-app.sh <program> <version> <out.app>   (signed, not notarised: for testing)
#
# Run it outside any sandbox: codesign and xcodebuild need the login keychain.
# The DMG is left unsigned on purpose: Gatekeeper checks the notarised app
# inside, while a signed-but-not-notarised DMG would itself be flagged.
set -euo pipefail
bin="$1"; ver="$2"; out="$3"
here="$(cd "$(dirname "$0")" && pwd)"
id="${SIGN_ID:-$(security find-identity -v -p codesigning | sed -nE 's/.*"(Developer ID Application: [^"]+)".*/\1/p' | head -1)}"
[ -n "$id" ] || { echo "No Developer ID Application certificate in the keychain"; exit 1; }
team="$(sed -nE 's/.*\(([A-Z0-9]{10})\)$/\1/p' <<< "$id")"
bundle="com.trykerf.bridge"

work="$(mktemp -d)"
archive="$work/KerfBridge.xcarchive"
app="$archive/Products/Applications/Kerf Bridge.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$bin" "$app/Contents/MacOS/kerf-bridge"
chmod 755 "$app/Contents/MacOS/kerf-bridge"

# The Kerf icon (AppIcon.png beside this script), at every size macOS asks for.
set_="$work/AppIcon.iconset"; mkdir "$set_"
for s in 16 32 128 256 512; do
    sips -z $s $s "$here/AppIcon.png" --out "$set_/icon_${s}x${s}.png" >/dev/null
    sips -z $((s*2)) $((s*2)) "$here/AppIcon.png" --out "$set_/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$set_" -o "$app/Contents/Resources/AppIcon.icns"

cat > "$app/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>$bundle</string>
  <key>CFBundleName</key><string>Kerf Bridge</string>
  <key>CFBundleDisplayName</key><string>Kerf Bridge</string>
  <key>CFBundleExecutable</key><string>kerf-bridge</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$ver</string>
  <key>CFBundleVersion</key><string>$ver</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <!-- A menu bar icon, no Dock icon. -->
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSLocalNetworkUsageDescription</key><string>Kerf Bridge announces itself on your network so Kerf on your iPad can find it, and LightBurn can connect to your laser.</string>
  <key>NSBonjourServices</key><array><string>_kerf._tcp</string></array>
</dict></plist>
EOF

echo "Signing as: $id"
codesign --force --options runtime --timestamp --sign "$id" "$app"
codesign --verify --strict --deep --verbose=2 "$app"
if [ -n "${SIGN_ONLY:-}" ]; then
    ditto "$app" "$out"
    echo "Wrote $out (signed, not notarised)"
    exit 0
fi

# An Xcode archive around the signed app, so xcodebuild can send it to Apple.
cat > "$archive/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>ApplicationProperties</key><dict>
    <key>ApplicationPath</key><string>Applications/Kerf Bridge.app</string>
    <key>Architectures</key><array><string>arm64</string><string>x86_64</string></array>
    <key>CFBundleIdentifier</key><string>$bundle</string>
    <key>CFBundleShortVersionString</key><string>$ver</string>
    <key>CFBundleVersion</key><string>$ver</string>
    <key>SigningIdentity</key><string>$id</string>
    <key>Team</key><string>$team</string>
  </dict>
  <key>ArchiveVersion</key><integer>2</integer>
  <key>CreationDate</key><date>$(date -u +%Y-%m-%dT%H:%M:%SZ)</date>
  <key>Name</key><string>Kerf Bridge</string>
  <key>SchemeName</key><string>Kerf Bridge</string>
</dict></plist>
EOF
cat > "$work/export.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>method</key><string>developer-id</string>
  <key>destination</key><string>upload</string>
  <key>teamID</key><string>$team</string>
  <key>signingStyle</key><string>manual</string>
  <key>signingCertificate</key><string>Developer ID Application</string>
</dict></plist>
EOF

echo "Uploading to Apple for notarisation..."
xcodebuild -exportArchive -archivePath "$archive" -exportOptionsPlist "$work/export.plist" \
           -exportPath "$work/upload" -allowProvisioningUpdates -quiet

# Apple usually answers in 1-15 minutes; the export succeeds once it has.
for i in $(seq 1 60); do
    if result="$(xcodebuild -exportNotarizedApp -archivePath "$archive" -exportPath "$work/notarized" 2>&1)"; then
        break
    fi
    if grep -qiE 'invalid|rejected' <<< "$result"; then echo "$result"; exit 1; fi
    [ "$i" -eq 60 ] && { echo "Apple did not finish within 30 minutes"; exit 1; }
    sleep 30
done
notarized="$work/notarized/Kerf Bridge.app"
xcrun stapler validate "$notarized"
spctl -a -t exec -vv "$notarized"

mkdir "$work/stage"
ditto "$notarized" "$work/stage/Kerf Bridge.app"
ln -s /Applications "$work/stage/Applications"
hdiutil create -volname "Kerf Bridge" -srcfolder "$work/stage" -format UDZO -ov "$out" -quiet
hdiutil verify "$out" -quiet
echo "Wrote $out (notarised, stapled)"
