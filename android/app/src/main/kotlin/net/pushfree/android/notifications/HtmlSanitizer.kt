package net.pushfree.android.notifications

import android.os.Build
import android.text.Html

/**
 * Minimal, injection-safe HTML sanitizer for notification text.
 *
 * Only `<b>`, `<i>`, `<u>` and `<a>` tags survive; every other tag is dropped
 * (script/style/iframe/object/embed blocks are removed together with their
 * content so payloads like `alert(...)` do not leak into the body). Anchor
 * `href` values are filtered to drop `javascript:`, `vbscript:` and `data:`
 * URLs. The sanitized string is then rendered through [Html.fromHtml] into a
 * styled [CharSequence] suitable for notification content.
 */
object HtmlSanitizer {
    private val ALLOWED = setOf("b", "i", "u", "a")
    private val BLOCK_TAGS = listOf("script", "style", "iframe", "object", "embed")

    /** Matches an opening or closing tag with optional attributes. */
    private val TAG_REGEX = Regex("(?is)<(/?)\\s*(\\w+)\\b([^>]*)>")

    /** Matches a dangerous URL scheme at the start of an href. */
    private val DANGEROUS_URL = Regex("(?i)^\\s*(javascript|vbscript|data)\\s*:")

    /** Matches an href attribute value (double- or single-quoted). */
    private val HREF_REGEX = Regex("(?i)href\\s*=\\s*[\"']([^\"']*)[\"']")

    /** Return the sanitized HTML string (still containing the allowed tags). */
    fun sanitize(input: String): String {
        if (input.isEmpty()) return input
        var s = input
        // 1. Remove dangerous *blocks* entirely (tag + inner text), plus any
        //    unbalanced leftovers of the same kind.
        for (tag in BLOCK_TAGS) {
            s = Regex("(?is)<$tag\\b[^>]*>.*?</$tag\\s*>").replace(s, "")
            s = Regex("(?is)</?$tag\\b[^>]*>").replace(s, "")
        }
        // 2. Drop every disallowed tag (keeping its surrounding text) and strip
        //    attributes from allowed tags, except a sanitized href on <a>.
        s = TAG_REGEX.replace(s) { match ->
            val closing = match.groupValues[1]
            val name = match.groupValues[2].lowercase()
            if (name !in ALLOWED) return@replace ""
            if (name == "a") {
                if (closing.isNotEmpty()) return@replace "</a>"
                val href = safeHref(match.groupValues[3]) ?: return@replace "<a>"
                return@replace "<a href=\"$href\">"
            }
            "<$closing$name>"
        }
        return s
    }

    /** Sanitize then render to a styled [CharSequence] for notification display. */
    fun render(input: String): CharSequence {
        if (input.isEmpty()) return input
        val sanitized = sanitize(input)
        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
            Html.fromHtml(sanitized, Html.FROM_HTML_MODE_COMPACT)
        } else {
            @Suppress("DEPRECATION")
            Html.fromHtml(sanitized)
        }
    }

    private fun safeHref(attrs: String): String? {
        val match = HREF_REGEX.find(attrs) ?: return null
        val raw = match.groupValues[1].trim()
        if (DANGEROUS_URL.containsMatchIn(raw)) return null
        return raw
    }
}
