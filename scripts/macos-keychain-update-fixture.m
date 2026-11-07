// Only used by macos-keychain-update-smoke.mjs with its newly created temporary keychain.
#import <Security/Security.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "../internal/platform/native_darwin.h"

#ifndef BUILD_VARIANT
#define BUILD_VARIANT 1
#endif
static volatile int buildVariant = BUILD_VARIANT;
void cr_tray_open(void) {}
void cr_tray_quit(void) {}
int main(int argc, char **argv) {
    if (argc != 3 || !strstr(argv[2], "codexrouter-keychain-update-") || !strstr(argv[2], "/synthetic.keychain-db")) return 2;
    void *keychain = NULL;
    const char *password = "CodexRouter-synthetic-fixture-only";
    int status;
    if (!strcmp(argv[1], "create")) {
        status = cr_keychain_fixture(argv[2], &keychain);
        if (!status) status = cr_keychain_set(keychain, "CodexRouter/synthetic-update-only", "synthetic", "synthetic-update-value", 22);
    } else {
        SecKeychainSetUserInteractionAllowed(false);
        status = (int)SecKeychainOpen(argv[2], (SecKeychainRef *)&keychain);
        if (!status) status = (int)SecKeychainUnlock((SecKeychainRef)keychain, (UInt32)strlen(password), password, true);
        if (!status && !strcmp(argv[1], "read")) {
            void *data = NULL; size_t length = 0;
            status = cr_keychain_get(keychain, "CodexRouter/synthetic-update-only", "synthetic", &data, &length);
            if (!status && (length != 22 || memcmp(data, "synthetic-update-value", 22))) status = -50;
            if (data) { memset(data, 0, length); cr_free(data); }
        } else if (!status && !strcmp(argv[1], "check")) {
            status = cr_keychain_check(keychain, "CodexRouter/synthetic-update-only", "synthetic", 0);
        } else if (!status && !strcmp(argv[1], "delete")) {
            status = cr_keychain_remove_fixture(keychain);
        } else if (!status) { status = -50; }
    }
    cr_release(keychain);
    printf("variant=%d status=%d\n", buildVariant, status);
    return 0;
}
