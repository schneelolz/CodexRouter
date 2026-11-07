//go:build darwin && cgo

#import <Security/Security.h>
#import <AppKit/AppKit.h>
#include <stdlib.h>
#include <stdatomic.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>
#include "native_darwin.h"

int cr_canonical_path(const char *path, char *buffer) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return errno;
    int status = fcntl(fd, F_GETPATH, buffer);
    int error = status < 0 ? errno : 0;
    close(fd);
    return error;
}

static NSMutableDictionary *query(void *keychain, const char *service, const char *name) {
    return [NSMutableDictionary dictionaryWithDictionary:@{
        (__bridge id)kSecClass: (__bridge id)kSecClassGenericPassword,
        (__bridge id)kSecAttrService: [NSString stringWithUTF8String:service],
        (__bridge id)kSecAttrAccount: [NSString stringWithUTF8String:name],
        (__bridge id)kSecMatchSearchList: @[(__bridge id)keychain],
        (__bridge id)kSecUseAuthenticationUI: (__bridge id)kSecUseAuthenticationUIFail
    }];
}
int cr_keychain_open(void **keychain) {
    // Process-local policy: a locked or denied keychain must fail, never prompt or fall back.
    OSStatus status = SecKeychainSetUserInteractionAllowed(false);
    if (status != errSecSuccess) return (int)status;
    return (int)SecKeychainCopyDefault((SecKeychainRef *)keychain);
}
int cr_keychain_get(void *keychain, const char *service, const char *name, void **data, size_t *length) {
    @autoreleasepool {
        NSMutableDictionary *q = query(keychain, service, name);
        q[(__bridge id)kSecReturnData] = @YES;
        CFTypeRef result = NULL;
        OSStatus status = SecItemCopyMatching((__bridge CFDictionaryRef)q, &result);
        if (status != errSecSuccess) return (int)status;
        NSData *value = (__bridge NSData *)result;
        if (![value isKindOfClass:[NSData class]] || value.length > 2560) { CFRelease(result); return errSecDecode; }
        *length = value.length;
        *data = malloc(value.length ? value.length : 1);
        if (!*data) { CFRelease(result); return errSecAllocate; }
        memcpy(*data, value.bytes, value.length);
        CFRelease(result);
        return 0;
    }
}
int cr_keychain_check(void *keychain, const char *service, const char *name, int authorize) {
    @autoreleasepool {
        // This is used only by an explicit local authorization command in a separate process.
        // No ACL, trusted application list or keychain default is changed programmatically.
        OSStatus status = SecKeychainSetUserInteractionAllowed(authorize != 0);
        if (status != errSecSuccess) return (int)status;
        NSMutableDictionary *q = query(keychain, service, name);
        if (authorize) q[(__bridge id)kSecUseAuthenticationUI] = (__bridge id)kSecUseAuthenticationUIAllow;
        q[(__bridge id)kSecReturnData] = @YES;
        CFTypeRef result = NULL;
        status = SecItemCopyMatching((__bridge CFDictionaryRef)q, &result);
        if (result) CFRelease(result); // Do not copy credential bytes to Go or print them.
        OSStatus restoreStatus = SecKeychainSetUserInteractionAllowed(false);
        return (int)(status != errSecSuccess ? status : restoreStatus);
    }
}
int cr_keychain_set(void *keychain, const char *service, const char *name, const void *data, size_t length) {
    @autoreleasepool {
        NSMutableDictionary *q = query(keychain, service, name);
        NSData *value = [NSData dataWithBytes:data length:length];
        OSStatus status = SecItemUpdate((__bridge CFDictionaryRef)q, (__bridge CFDictionaryRef)@{(__bridge id)kSecValueData: value});
        if (status != errSecItemNotFound) return (int)status;
        [q removeObjectForKey:(__bridge id)kSecMatchSearchList];
        q[(__bridge id)kSecUseKeychain] = (__bridge id)keychain;
        q[(__bridge id)kSecValueData] = value;
        return (int)SecItemAdd((__bridge CFDictionaryRef)q, NULL);
    }
}
int cr_keychain_delete(void *keychain, const char *service, const char *name) {
    @autoreleasepool { return (int)SecItemDelete((__bridge CFDictionaryRef)query(keychain, service, name)); }
}
void cr_release(void *object) { if (object) CFRelease(object); }
void cr_free(void *data) { free(data); }
// Explicit fixture paths; no default-keychain or search-list setter is called.
int cr_keychain_fixture(const char *path, void **keychain) {
    OSStatus status = SecKeychainSetUserInteractionAllowed(false);
    if (status != errSecSuccess) return (int)status;
    const char *password = "CodexRouter-synthetic-fixture-only";
    return (int)SecKeychainCreate(path, (UInt32)strlen(password), password, false, NULL, (SecKeychainRef *)keychain);
}
int cr_keychain_lock_fixture(void *keychain) { return (int)SecKeychainLock((SecKeychainRef)keychain); }
int cr_keychain_remove_fixture(void *keychain) { return (int)SecKeychainDelete((SecKeychainRef)keychain); }

@interface CRMenu : NSObject
- (void)open:(id)sender;
- (void)quit:(id)sender;
@end
@implementation CRMenu
- (void)open:(id)sender { cr_tray_open(); }
- (void)quit:(id)sender { cr_tray_quit(); }
@end
static atomic_bool stopping = false;
int cr_tray_run(void) {
    @autoreleasepool {
        if (![NSThread isMainThread]) return -1;
        NSApplication *app = [NSApplication sharedApplication];
        [app setActivationPolicy:NSApplicationActivationPolicyAccessory];
        CRMenu *actions = [CRMenu new];
        NSStatusItem *item = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
        item.button.image = [NSImage imageWithSystemSymbolName:@"arrow.triangle.branch" accessibilityDescription:@"CodexRouter"];
        item.button.toolTip = @"CodexRouter - Running (loopback only)";
        NSMenu *menu = [NSMenu new];
        NSMenuItem *status = [menu addItemWithTitle:@"CodexRouter: Running" action:nil keyEquivalent:@""];
        status.enabled = NO;
        NSMenuItem *open = [menu addItemWithTitle:@"Open Management" action:@selector(open:) keyEquivalent:@""];
        open.target = actions;
        [menu addItem:[NSMenuItem separatorItem]];
        NSMenuItem *quit = [menu addItemWithTitle:@"Quit CodexRouter" action:@selector(quit:) keyEquivalent:@""];
        quit.target = actions;
        item.menu = menu;
        [app finishLaunching];
        // Manual event loop allows context cancellation before AppKit starts running.
        while (!atomic_load(&stopping)) {
            @autoreleasepool {
                NSEvent *event = [app nextEventMatchingMask:NSEventMaskAny untilDate:[NSDate dateWithTimeIntervalSinceNow:0.1] inMode:NSDefaultRunLoopMode dequeue:YES];
                if (event) [app sendEvent:event];
                [app updateWindows];
            }
        }
        [[NSStatusBar systemStatusBar] removeStatusItem:item];
        [actions release]; [menu release];
        return 0;
    }
}
void cr_tray_stop(void) { atomic_store(&stopping, true); }
