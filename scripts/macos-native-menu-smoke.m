// Exercises the real AppKit menu in an isolated test process. No Accessibility permission,
// user process automation, input injection or desktop capture is involved.
#import <AppKit/AppKit.h>
#import <objc/runtime.h>
#include "../internal/platform/native_darwin.h"

static NSMenu *ownedMenu;
static void (*originalSetMenu)(id, SEL, NSMenu *);
static int opened, quitCount, failures;
static void captureMenu(id item, SEL selector, NSMenu *menu) {
    if ([menu.itemArray.firstObject.title isEqualToString:@"CodexRouter: Running"]) ownedMenu = menu;
    originalSetMenu(item, selector, menu);
}
void cr_tray_open(void) { opened++; }
void cr_tray_quit(void) { quitCount++; cr_tray_stop(); }
static void require(BOOL condition) { if (!condition) failures++; }

int main(void) {
    @autoreleasepool {
        Method method = class_getInstanceMethod([NSStatusItem class], @selector(setMenu:));
        if (!method) return 2;
        originalSetMenu = (void *)method_getImplementation(method);
        method_setImplementation(method, (IMP)captureMenu);
        [NSTimer scheduledTimerWithTimeInterval:0.4 repeats:NO block:^(NSTimer *timer) {
            require(ownedMenu != nil);
            if (!ownedMenu) { cr_tray_stop(); return; }
            require(ownedMenu.numberOfItems == 4);
            require(![ownedMenu itemAtIndex:0].enabled);
            require([[ownedMenu itemAtIndex:1].title isEqualToString:@"Open Management"]);
            require([[ownedMenu itemAtIndex:3].title isEqualToString:@"Quit CodexRouter"]);
            [ownedMenu performActionForItemAtIndex:1];
            require(opened == 1 && quitCount == 0);
            [ownedMenu performActionForItemAtIndex:3];
            require(opened == 1 && quitCount == 1);
        }];
        int result = cr_tray_run();
        method_setImplementation(method, (IMP)originalSetMenu);
        require(result == 0 && opened == 1 && quitCount == 1);
        if (failures) { fprintf(stderr, "native menu assertions failed: %d\n", failures); return 1; }
        puts("native AppKit menu status, Open Management and Quit action dispatch passed");
        return 0;
    }
}
