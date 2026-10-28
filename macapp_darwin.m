// The Mac menu bar for Kerf Bridge.app; macapp_darwin.go drives it.

#import <Cocoa/Cocoa.h>
#import <ServiceManagement/ServiceManagement.h>

extern void goQuit(void);

static NSMenuItem *statusLine;
static NSMenuItem *loginItem;
static NSString *pageURL;

@interface KerfMenu : NSObject
@end

@implementation KerfMenu
- (void)openPage:(id)sender {
	[[NSWorkspace sharedWorkspace] openURL:[NSURL URLWithString:pageURL]];
}
- (void)toggleLogin:(id)sender API_AVAILABLE(macos(13.0)) {
	SMAppService *svc = [SMAppService mainAppService];
	NSError *err = nil;
	if (svc.status == SMAppServiceStatusEnabled) {
		[svc unregisterAndReturnError:&err];
	} else {
		[svc registerAndReturnError:&err];
	}
	if (err) NSLog(@"Kerf Bridge: start at login: %@", err);
	loginItem.state = svc.status == SMAppServiceStatusEnabled ? NSControlStateValueOn : NSControlStateValueOff;
}
- (void)quit:(id)sender {
	goQuit();
}
@end

static KerfMenu *target;
static NSStatusItem *item;

// runMenuBar shows the menu bar icon and runs the app until Quit. It must be
// called on the main thread, and doesn't return.
void runMenuBar(const char *url, const char *version) {
	@autoreleasepool {
		pageURL = [NSString stringWithUTF8String:url];
		NSApplication *app = [NSApplication sharedApplication];
		[app setActivationPolicy:NSApplicationActivationPolicyAccessory];
		target = [KerfMenu new];

		item = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
		NSImage *img = [NSImage imageWithSystemSymbolName:@"bolt.horizontal.fill" accessibilityDescription:@"Kerf Bridge"];
		if (img) {
			img.template = YES;
			item.button.image = img;
		} else {
			item.button.title = @"Kerf";
		}

		NSMenu *menu = [NSMenu new];
		NSMenuItem *title = [[NSMenuItem alloc] initWithTitle:[NSString stringWithFormat:@"Kerf Bridge %s", version] action:nil keyEquivalent:@""];
		title.enabled = NO;
		[menu addItem:title];
		statusLine = [[NSMenuItem alloc] initWithTitle:@"Looking for the laser…" action:nil keyEquivalent:@""];
		statusLine.enabled = NO;
		[menu addItem:statusLine];
		[menu addItem:[NSMenuItem separatorItem]];

		NSMenuItem *open = [[NSMenuItem alloc] initWithTitle:@"Open Kerf Bridge…" action:@selector(openPage:) keyEquivalent:@"o"];
		open.target = target;
		[menu addItem:open];
		if (@available(macOS 13.0, *)) {
			loginItem = [[NSMenuItem alloc] initWithTitle:@"Start at Login" action:@selector(toggleLogin:) keyEquivalent:@""];
			loginItem.target = target;
			loginItem.state = [SMAppService mainAppService].status == SMAppServiceStatusEnabled ? NSControlStateValueOn : NSControlStateValueOff;
			[menu addItem:loginItem];
		}
		[menu addItem:[NSMenuItem separatorItem]];
		NSMenuItem *quit = [[NSMenuItem alloc] initWithTitle:@"Quit Kerf Bridge" action:@selector(quit:) keyEquivalent:@"q"];
		quit.target = target;
		[menu addItem:quit];
		item.menu = menu;

		[app run];
	}
}

void setStatusLine(const char *text) {
	NSString *s = [NSString stringWithUTF8String:text];
	dispatch_async(dispatch_get_main_queue(), ^{ statusLine.title = s; });
}

void openPageNow(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[[NSWorkspace sharedWorkspace] openURL:[NSURL URLWithString:pageURL]];
	});
}
