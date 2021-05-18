#define _DEFAULT_SOURCE

#include <dirent.h>
#include <glib.h>
#include <stdbool.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#include "onedriver.h"
#include "systemd.h"

/**
 * Block until the fs is available, or a timeout is reached. If the timeout is
 * -1, will wait until a default of 120 seconds.
 */
void fs_poll_until_avail(const char *mountpoint, int timeout) {
    bool found = false;
    if (timeout == -1) {
        timeout = 120;
    }
    for (int i = 0; i < timeout * 10; i++) {
        DIR *dir = opendir(mountpoint);
        if (!dir) {
            return;
        }
        struct dirent *entry;
        while ((entry = readdir(dir)) != NULL) {
            if (strcmp(entry->d_name, XDG_VOLUME_INFO) == 0) {
                found = true;
                break;
            }
        }
        closedir(dir);

        if (found) {
            break;
        }
        usleep(100 * 1000); // 0.1 seconds
    }
}

/**
 * Grab the FS account name from .xdg-volume-info. Returned value should be freed by
 * caller.
 */
char *fs_account_name(const char *mount_name) {
    int mount_len = strlen(mount_name);
    char fname[mount_len + strlen(XDG_VOLUME_INFO) + 2];
    strcpy((char *)&fname, mount_name);
    strcat((char *)&fname, "/");
    strcat((char *)&fname, XDG_VOLUME_INFO);
    FILE *file = fopen(fname, "r");
    if (file == NULL) {
        g_error("Could not open file %s\n", fname);
        return NULL;
    }

    char *account_name = NULL;
    char *line = NULL;
    size_t len = 0;
    while (getline(&line, &len, file) != -1) {
        if (strncmp(line, "Name=", 5) == 0) {
            account_name = strdup(line + 5);
            account_name[strlen(account_name) - 1] = '\0'; // get rid of newline
            break;
        }
    }
    fclose(file);
    return account_name;
}

/**
 * Check that the mountpoint is actually valid: mounpoint exists and nothing is in it.
 */
bool fs_mountpoint_is_valid(const char *mountpoint) {
    if (!mountpoint || !strlen(mountpoint)) {
        return false;
    }

    bool valid = true;
    DIR *dir = opendir(mountpoint);
    if (!dir) {
        return false;
    }
    struct dirent *entry;
    while ((entry = readdir(dir)) != NULL) {
        if (strcmp(entry->d_name, ".") == 0 || strcmp(entry->d_name, "..") == 0) {
            continue;
        }
        valid = false;
        break;
    }
    closedir(dir);

    return valid;
}

/**
 * Get a null-terminated array of strings, each corresponding to the path of a mountpoint.
 * These are detected from the folder names of onedriver's cache dir.;
 */
char **fs_known_mounts() {
    char *cachedir = malloc(strlen(g_get_user_cache_dir()) + strlen(ONEDRIVER_NAME) + 2);
    strcat(strcat(strcpy(cachedir, g_get_user_cache_dir()), "/"), ONEDRIVER_NAME);
    DIR *cache = opendir(cachedir);
    if (!cache) {
        return NULL;
    }
    free(cachedir);

    int idx = 0;
    int size = 10;
    char **r = calloc(sizeof(char *), size);
    struct dirent *entry;
    while ((entry = readdir(cache)) != NULL) {
        if (entry->d_type & DT_DIR && entry->d_name[0] != '.') {
            // unescape the systemd unit name of all folders in cache directory
            char *path = systemd_unescape((const char *)&(entry->d_name));
            char *fullpath = malloc(strlen(path) + 2);
            memcpy(fullpath, "/\0", 2);
            strcat(fullpath, path);
            free(path);

            // do the mountpoints they point to actually exist?
            struct stat st;
            if (stat(fullpath, &st) == 0 && st.st_mode & S_IFDIR) {
                // yep, add em
                r[idx++] = fullpath;
                if (idx > size) {
                    size *= 2;
                    r = realloc(r, size * sizeof(char *));
                }
            }
        }
    }
    r[idx] = NULL;
    return r;
}

/**
 * Strip the /home/username part from a path and replace it with "~". Result should be
 * freed by caller.
 */
char *escape_home(const char *path) {
    const char *homedir = g_get_home_dir();
    int len = strlen(homedir);
    if (strncmp(path, homedir, len) == 0) {
        char *replaced = strdup(path + len - 1);
        replaced[0] = '~';
        return replaced;
    }
    return strdup(path);
}

/**
 * Replace the tilde in a path with the absolute path
 */
char *unescape_home(const char *path) {
    if (path[0] == '/') {
        return strdup(path);
    }
    const char *homedir = g_get_home_dir();
    int len = strlen(homedir);
    char *new_path = malloc(strlen(path) - 1 + len);
    return strcat(strcpy(new_path, homedir), path + 1);
}