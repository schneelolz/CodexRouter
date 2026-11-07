//go:build darwin && cgo

#ifndef CODEXROUTER_DARWIN_H
#define CODEXROUTER_DARWIN_H
#include <stddef.h>
int cr_canonical_path(const char *path, char *buffer);
int cr_keychain_open(void **keychain);
int cr_keychain_get(void *keychain, const char *service, const char *name, void **data, size_t *length);
int cr_keychain_check(void *keychain, const char *service, const char *name, int authorize);
int cr_keychain_set(void *keychain, const char *service, const char *name, const void *data, size_t length);
int cr_keychain_delete(void *keychain, const char *service, const char *name);
void cr_release(void *object);
void cr_free(void *data);
int cr_keychain_fixture(const char *path, void **keychain);
int cr_keychain_remove_fixture(void *keychain);
int cr_keychain_lock_fixture(void *keychain);
int cr_tray_run(void);
void cr_tray_stop(void);
void cr_tray_open(void);
void cr_tray_quit(void);
#endif
