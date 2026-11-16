/*
 * Lets pages in the kiosk's browser (cog) start sound by themselves, the way Chromium's
 * --autoplay-policy=no-user-gesture-required did.
 *
 * WebKit decides this per page from the view's "website policies", whose autoplay policy is "allow, but
 * without sound" unless the program that makes the view says otherwise. cog doesn't, and has no option for it
 * (its --media-playback-requires-user-gesture is overruled by that policy). So this library, loaded into cog with
 * LD_PRELOAD, stands in front of GObject's g_object_new: when WebKit makes its default WebKitWebsitePolicies
 * (g_object_new(WEBKIT_TYPE_WEBSITE_POLICIES, NULL)), it gets one with autoplay allowed. Everything else is
 * passed on unchanged.
 *
 * No headers needed: cc -shared -fPIC -O2 -o jester-autoplay.so jester-autoplay.c
 */
#include <stdarg.h>
#include <string.h>

typedef unsigned long GType; /* gsize */

extern const char *g_type_name(GType type);
extern void *g_object_new_valist(GType type, const char *first_property_name, va_list args);
extern void *g_object_new_with_properties(GType type, unsigned int n, const char *names[], const void *values);

static void *make(GType type, const char *first_property_name, ...)
{
    va_list args;
    va_start(args, first_property_name);
    void *object = g_object_new_valist(type, first_property_name, args);
    va_end(args);
    return object;
}

void *g_object_new(GType type, const char *first_property_name, ...)
{
    if (!first_property_name) {
        const char *name = g_type_name(type);
        if (name && !strcmp(name, "WebKitWebsitePolicies"))
            return make(type, "autoplay", 0 /* WEBKIT_AUTOPLAY_ALLOW */, (char *) 0);
        return g_object_new_with_properties(type, 0, 0, 0);
    }
    va_list args;
    va_start(args, first_property_name);
    void *object = g_object_new_valist(type, first_property_name, args);
    va_end(args);
    return object;
}
