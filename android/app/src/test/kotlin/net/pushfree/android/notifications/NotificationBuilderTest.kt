package net.pushfree.android.notifications

import android.app.Notification
import android.app.NotificationManager
import android.content.Context
import android.graphics.Bitmap
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import net.pushfree.android.data.AckState
import net.pushfree.android.data.MessageEntity
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.annotation.Config

/**
 * Robolectric tests for the notification pipeline. Covers the deliverable
 * matrix: priority->channel mapping, emergency IMPORTANCE_MAX, the API-34
 * full-screen-intent permission branch, attachment failure -> text-only
 * fallback, HTML sanitization (script neutralized) and notification id == send_id.
 */
@RunWith(AndroidJUnit4::class)
@Config(sdk = [34])
class NotificationBuilderTest {

    private lateinit var context: Context
    private lateinit var builder: PushfreeNotificationBuilder

    @Before
    fun setUp() {
        context = ApplicationProvider.getApplicationContext()
        // Default loader always fails; success cases inject their own loader.
        builder = PushfreeNotificationBuilder(
            context,
            attachmentLoader = AttachmentLoader { null },
        )
    }

    private fun msg(
        priority: Int,
        sendId: Long = 77001L,
        receiptId: String? = null,
        attachmentUri: String? = null,
        body: String = "body text",
        title: String? = "title",
    ) = MessageEntity(
        id = sendId,
        sub = "https://srv",
        sendId = sendId,
        title = title,
        body = body,
        priority = priority,
        attachmentUri = attachmentUri,
        ackState = if (receiptId != null) AckState.PENDING else AckState.NONE,
        receiptId = receiptId,
    )

    private fun Notification.bigPictureBitmap(): Bitmap? =
        extras.get(Notification.EXTRA_PICTURE) as? Bitmap

    /** True when a BigPictureStyle was applied (template, raw bitmap, or picture Icon). */
    private fun Notification.hasBigPicture(): Boolean {
        val template = extras.getString(Notification.EXTRA_TEMPLATE)
        if (template?.contains("BigPictureStyle") == true) return true
        if (extras.get(Notification.EXTRA_PICTURE) != null) return true
        if (extras.get(Notification.EXTRA_PICTURE_ICON) != null) return true
        return false
    }

    private fun Notification.titleText(): CharSequence? = extras.getCharSequence(Notification.EXTRA_TITLE)
    private fun Notification.bodyText(): CharSequence? = extras.getCharSequence(Notification.EXTRA_TEXT)

    // 1. Priority -> channel matrix: all four rows.
    @Test
    fun priority_matrix_maps_all_four_priorities() {
        val silent = Notifications.channelSpecFor(-1)
        val default = Notifications.channelSpecFor(0)
        val high = Notifications.channelSpecFor(1)
        val emergency = Notifications.channelSpecFor(2)

        assertEquals(NotificationTier.SILENT, silent.tier)
        assertEquals(Notifications.CHANNEL_SILENT, silent.channelId)
        assertEquals(NotificationManager.IMPORTANCE_LOW, silent.importance)
        assertFalse("silent must not vibrate", silent.vibrate)
        assertFalse("silent must not request FSI", silent.wantsFullScreenIntent)

        assertEquals(NotificationTier.DEFAULT, default.tier)
        assertEquals(Notifications.CHANNEL_DEFAULT, default.channelId)
        assertEquals(NotificationManager.IMPORTANCE_DEFAULT, default.importance)

        assertEquals(NotificationTier.HIGH, high.tier)
        assertEquals(Notifications.CHANNEL_HIGH, high.channelId)
        assertEquals(NotificationManager.IMPORTANCE_HIGH, high.importance)

        assertEquals(NotificationTier.EMERGENCY, emergency.tier)
        assertEquals(Notifications.CHANNEL_EMERGENCY, emergency.channelId)
    }

    @Test
    fun out_of_range_priority_clamps() {
        assertEquals(NotificationTier.SILENT, Notifications.channelSpecFor(-2).tier)
        assertEquals(NotificationTier.EMERGENCY, Notifications.channelSpecFor(3).tier)
    }

    // 2. Emergency = IMPORTANCE_MAX asserted, with FSI + vibration.
    @Test
    fun emergency_priority_uses_max_importance() {
        val spec = Notifications.channelSpecFor(2)
        assertEquals(NotificationManager.IMPORTANCE_MAX, spec.importance)
        assertTrue(spec.wantsFullScreenIntent)
        assertTrue(spec.vibrate)
    }

    // 3a. API-34 FSI branch: granted -> full-screen intent set.
    @Test
    fun api34_emergency_with_fsi_granted_sets_full_screen_intent() {
        val result = builder.build(
            msg(priority = 2, receiptId = "r1"),
            apiLevel = 34,
            canUseFullScreenIntent = true,
        )
        assertEquals(NotificationTier.EMERGENCY, result.tier)
        assertTrue("FSI used when granted", result.usedFullScreenIntent)
        assertFalse("no fallback vibration when FSI used", result.usedFallbackVibration)
        assertNotNull(result.notification.fullScreenIntent)
        assertFalse("no settings request when granted", result.requestFullScreenIntentSettings)
    }

    // 3b. API-34 FSI branch: denied -> heads-up + strong vibration, NO FSI, never silent.
    @Test
    fun api34_emergency_without_fsi_falls_back_to_heads_up_vibration() {
        val result = builder.build(
            msg(priority = 2, receiptId = "r1"),
            apiLevel = 34,
            canUseFullScreenIntent = false,
        )
        assertEquals(NotificationTier.EMERGENCY, result.tier)
        assertEquals(Notifications.CHANNEL_EMERGENCY, result.channelId)
        assertFalse("FSI not used when denied", result.usedFullScreenIntent)
        assertNull("no fullScreenIntent when denied", result.notification.fullScreenIntent)
        assertTrue("fallback vibration applied", result.usedFallbackVibration)
        assertNotNull("vibration pattern set", result.notification.vibrate)
        assertTrue("pattern non-empty", result.notification.vibrate!!.isNotEmpty())
        assertArrayEquals(
            Notifications.VIBRATION_PATTERN_EMERGENCY,
            result.notification.vibrate,
        )
        assertEquals(Notification.CATEGORY_ALARM, result.notification.category)
        assertTrue("settings intent should be surfaced", result.requestFullScreenIntentSettings)
    }

    @Test
    fun below_api34_emergency_uses_full_screen_intent_unconditionally() {
        // FSI grant is irrelevant below API 34: used regardless.
        val result = builder.build(
            msg(priority = 2, receiptId = "r1"),
            apiLevel = 33,
            canUseFullScreenIntent = false,
        )
        assertTrue(result.usedFullScreenIntent)
        assertNotNull(result.notification.fullScreenIntent)
        assertFalse(result.requestFullScreenIntentSettings)
    }

    // 4. Attachment failure -> text-only notification still posted.
    @Test
    fun attachment_load_failure_keeps_text_only_notification() {
        val failing = AttachmentLoader { null }
        val b = PushfreeNotificationBuilder(context, attachmentLoader = failing)
        val result = b.build(
            msg(priority = 1, attachmentUri = "https://example.com/img.png"),
            apiLevel = 34,
            canUseFullScreenIntent = true,
        )
        assertFalse("attachment not rendered on failure", result.attachmentRendered)
        assertFalse("no BigPicture style on failure", result.notification.hasBigPicture())
        assertNull("no raw bitmap on failure", result.notification.bigPictureBitmap())
        assertEquals("body text", result.notification.bodyText()?.toString())
        assertEquals("title", result.notification.titleText()?.toString())
    }

    @Test
    fun attachment_success_renders_big_picture() {
        val bmp = Bitmap.createBitmap(8, 8, Bitmap.Config.ARGB_8888)
        val b = PushfreeNotificationBuilder(context, attachmentLoader = AttachmentLoader { bmp })
        val result = b.build(
            msg(priority = 1, attachmentUri = "https://example.com/img.png"),
            apiLevel = 34,
            canUseFullScreenIntent = true,
        )
        assertTrue(result.attachmentRendered)
        assertTrue("BigPictureStyle applied", result.notification.hasBigPicture())
        assertNotNull("picture bitmap present in extras", result.notification.bigPictureBitmap())
    }

    // 5. HTML sanitization: b/i/u/a kept; script neutralized.
    @Test
    fun html_sanitizer_neutralizes_script_and_keeps_safe_tags() {
        val raw = "<b>bold</b> <i>it</i> <u>u</u> <a href=\"https://ok.example\">link</a> " +
            "<script>alert(1)</script> plain <img src=x onerror=alert(2)> <div>div</div>"
        val out = HtmlSanitizer.sanitize(raw).lowercase()
        assertTrue("keeps <b>", out.contains("<b>"))
        assertTrue("keeps <i>", out.contains("<i>"))
        assertTrue("keeps <u>", out.contains("<u>"))
        assertTrue("keeps safe href", out.contains("https://ok.example"))
        assertFalse("script removed", out.contains("script"))
        assertFalse("img removed", out.contains("img"))
        assertFalse("onerror removed", out.contains("onerror"))
        assertFalse("div removed", out.contains("<div"))
        assertFalse("alert payload removed", out.contains("alert"))
    }

    @Test
    fun html_sanitizer_drops_javascript_href() {
        val out = HtmlSanitizer.sanitize("<a href=\"javascript:alert(1)\">x</a>")
        assertFalse(out.lowercase().contains("javascript"))
        assertTrue("anchor kept without href", out.contains("<a>"))
    }

    @Test
    fun html_sanitizer_renders_to_styled_text() {
        val rendered = HtmlSanitizer.render("<b>hi</b>")
        assertEquals("hi", rendered.toString())
    }

    // 6. Notification id == send_id.
    @Test
    fun notification_id_equals_send_id() {
        val result = builder.build(
            msg(priority = 0, sendId = 55599L),
            apiLevel = 34,
            canUseFullScreenIntent = true,
        )
        assertEquals(55599, result.notificationId)
        assertEquals(msg(priority = 0, sendId = 55599L).sendId.toInt(), result.notificationId)
    }

    // Happy path: p2 -> emergency channel + ack button present.
    @Test
    fun happy_path_p2_emergency_channel_with_ack_action() {
        val result = builder.build(
            msg(priority = 2, receiptId = "rec-42"),
            apiLevel = 34,
            canUseFullScreenIntent = true,
        )
        assertEquals(NotificationTier.EMERGENCY, result.tier)
        assertEquals(Notifications.CHANNEL_EMERGENCY, result.channelId)
        val actions = result.notification.actions
        assertNotNull("actions array present", actions)
        assertEquals("ack action present", 1, actions!!.size)
        assertEquals("Acknowledge", actions[0].title.toString())
    }

    @Test
    fun no_ack_action_without_receipt() {
        val result = builder.build(
            msg(priority = 1),
            apiLevel = 34,
            canUseFullScreenIntent = true,
        )
        val actions = result.notification.actions
        assertTrue("no ack action without receipt", actions == null || actions.isEmpty())
    }

    @Test
    fun ensure_channels_creates_all_four_channels() {
        Notifications.ensureChannels(context)
        val nm = context.getSystemService(NotificationManager::class.java)
        val ids = nm.notificationChannels.map { it.id }.toSet()
        assertTrue(ids.contains(Notifications.CHANNEL_SILENT))
        assertTrue(ids.contains(Notifications.CHANNEL_DEFAULT))
        assertTrue(ids.contains(Notifications.CHANNEL_HIGH))
        assertTrue(ids.contains(Notifications.CHANNEL_EMERGENCY))
        val emergency = nm.getNotificationChannel(Notifications.CHANNEL_EMERGENCY)
        assertEquals(NotificationManager.IMPORTANCE_MAX, emergency.importance)
        assertTrue("emergency channel vibrates", emergency.shouldVibrate())
        val silent = nm.getNotificationChannel(Notifications.CHANNEL_SILENT)
        assertEquals(NotificationManager.IMPORTANCE_LOW, silent.importance)
    }

    @Test
    fun builder_uses_real_device_defaults_never_throws() {
        // Exercises the single-arg build() path (real API + FSI resolution).
        val result = builder.build(msg(priority = 0, body = "<b>safe</b>"))
        assertEquals(NotificationTier.DEFAULT, result.tier)
        assertEquals(Notifications.CHANNEL_DEFAULT, result.channelId)
    }
}
