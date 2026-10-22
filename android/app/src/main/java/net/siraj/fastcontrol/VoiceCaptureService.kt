package net.siraj.fastcontrol

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.Uri
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.PowerManager
import org.json.JSONArray
import org.json.JSONObject

/** Owns a user-started recording independently of the Activity and WebView. */
class VoiceCaptureService : Service() {
    companion object {
        const val START = "net.siraj.fastcontrol.voice.START"
        const val RETRY = "net.siraj.fastcontrol.voice.RETRY"
        private const val FINISH = "net.siraj.fastcontrol.voice.FINISH"
        private const val CANCEL = "net.siraj.fastcontrol.voice.CANCEL"
        private const val CHANNEL = "voice-recording"
        private const val NOTIFICATION = 3
        private const val RESULT = "result"
        @Volatile private var instance: VoiceCaptureService? = null
        // Registration and delivery run on Android's main thread. Payloads are immutable strings.
        var observer: ((String) -> Unit)? = null
        val isRecording: Boolean get() = instance?.recorder?.isRecording == true
        private fun prefs(ctx: Context) = ctx.getSharedPreferences("voice-session", Context.MODE_PRIVATE)

        fun snapshot(ctx: Context): String = instance?.snapshotJSON?.takeIf { it != "{}" }
            ?: prefs(ctx).getString(RESULT, "{}") ?: "{}"

        fun acknowledge(ctx: Context, id: String) {
            val service = instance
            if (service?.state?.optString("id") == id && service.terminal()) service.snapshotJSON = "{}"
            val saved = try { JSONObject(prefs(ctx).getString(RESULT, "{}") ?: "{}") } catch (_: Exception) { JSONObject() }
            if (saved.optString("id") == id) prefs(ctx).edit().remove(RESULT).apply()
        }

        fun discardSaved(ctx: Context, id: String) {
            val saved = try {JSONObject(snapshot(ctx))}catch (_: Exception){return}
            if (saved.optString("id") != id || saved.optString("phase") != "error" || !saved.optBoolean("retryable")) return
            VoiceRecorder.savedFile(ctx,saved.optString("audioPath"))?.delete()
            prefs(ctx).edit().remove(RESULT).apply()
            instance?.let { if(it.state.optString("id")==id && it.terminal()){it.state.put("phase","cancelled");it.snapshotJSON="{}";it.stopWhenIdle()} }
        }

        private val cancelledStarts = linkedSetOf<String>()
        fun finish(id: String, server: Server, base: String) {
            instance?.let { if (it.state.optString("id") == id) it.finishCapture(server, base) }
        }
        fun cancel(id: String) {
            if (id.isEmpty()) return
            cancelledStarts.add(id)
            while (cancelledStarts.size > 32) cancelledStarts.remove(cancelledStarts.first())
            instance?.let { if (it.state.optString("id") == id) it.cancelCapture() }
        }
        fun updateDraft(id: String, text: String) {
            instance?.let {
                if (it.state.optString("id") == id && !it.terminal()) {
                    it.state.put("draft", text.take(65536)); it.publish()
                }
            }
        }
        fun failStart(ctx: Context, state: JSONObject, message: String) {
            val event = JSONObject(state.toString()).put("type", "error").put("phase", "error").put("message", message).toString()
            prefs(ctx).edit().putString(RESULT, event).apply()
            observer?.invoke(event)
        }
    }

    private val main = Handler(Looper.getMainLooper())
    private lateinit var recorder: VoiceRecorder
    private lateinit var notifications: NotificationManager
    private var state = JSONObject()
    @Volatile private var snapshotJSON = "{}"
    private var lastStartId = 0
    private var lastNotificationSecond = -1
    private var wake: PowerManager.WakeLock? = null
    private val levels = ArrayDeque<Double>()

    override fun onCreate() {
        super.onCreate()
        notifications = getSystemService(NotificationManager::class.java)
        notifications.createNotificationChannel(NotificationChannel(CHANNEL, "Voice Recording", NotificationManager.IMPORTANCE_LOW).apply {
            description = "Recording and transcribing a voice draft"
            setSound(null, null)
        })
        recorder = VoiceRecorder(applicationContext, { main.post {
            if (instance !== this) return@post
            observer?.invoke(JSONObject().put("type", "captureState").toString())
            stopWhenIdle()
        } }) { event ->
            val json = event.toString()
            main.post { if (instance === this) onVoiceEvent(JSONObject(json)) }
        }
        instance = this
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        lastStartId = startId
        when (intent?.action) {
            START -> startCapture(intent)
            RETRY -> retryCapture(intent)
            FINISH -> if (intent.getStringExtra("id") == state.optString("id")) {
                val server = Store.servers(this).find { it.id == state.optString("server") }
                val base = state.optString("base")
                if (server != null && base in server.urls.map { it.trimEnd('/') }) finishCapture(server, base)
                else failAndCancel("Reconnect To The Computer And Try Again")
            }
            CANCEL -> if (intent.getStringExtra("id") == state.optString("id")) cancelCapture()
            else -> if (!recorder.isBusy) stopSelfResult(startId)
        }
        if (state.optString("id").isEmpty() && !recorder.isBusy) stopSelfResult(startId)
        return START_NOT_STICKY
    }

    private fun startCapture(intent: Intent) {
        val requested = try { JSONObject(intent.getStringExtra("session") ?: "{}") } catch (_: Exception) { stopSelfResult(lastStartId); return }
        if (recorder.isBusy || (!terminal() && state.optString("id").isNotEmpty())) {
            if (terminal()) failStart(this, requested, "Finishing The Previous Recording. Try Again")
            else observer?.invoke(JSONObject().put("type", "captureState").toString())
            return
        }
        state = requested
        state.put("phase", "preparing").put("type", "preparing").put("seconds", 0)
        levels.clear(); lastNotificationSecond = -1
        publish()
        try {
            val notice = notification()
            if (Build.VERSION.SDK_INT >= 30) startForeground(NOTIFICATION, notice, ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE)
            else startForeground(NOTIFICATION, notice)
            wake = getSystemService(PowerManager::class.java).newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "fastcontrol:voice").apply {
                acquire()
            }
            if (cancelledStarts.remove(state.getString("id"))) {
                cancelCapture(); return
            }
            recorder.start(state.getString("id"), intent.getBooleanExtra("compressed", false))
            observer?.invoke(JSONObject().put("type", "captureState").toString())
        } catch (_: Exception) { failAndCancel("Could Not Start Background Recording. Return To fastcontrol And Try Again") }
    }

    private fun publish() { snapshotJSON = JSONObject(state.toString()).put("levels", JSONArray(levels.toList())).toString() }
    private fun terminal() = state.optString("phase") in setOf("transcript", "error", "cancelled")

    private fun retryCapture(intent: Intent) {
        val requested = try {JSONObject(intent.getStringExtra("session") ?: "{}")}catch (_: Exception){stopSelfResult(lastStartId);return}
        if(recorder.isBusy || requested.optString("phase")!="error" || !requested.optBoolean("retryable")) return
        val server=Store.servers(this).find {it.id==requested.optString("server")}
        val base=requested.optString("base")
        state=requested
        if(server==null || base !in server.urls.map{it.trimEnd('/')}) {failStart(this,state,"Reconnect To The Computer And Retry");stopSelfResult(lastStartId);return}
        if(VoiceRecorder.savedFile(this,state.optString("audioPath"))==null) {state.put("retryable",false);failStart(this,state,"Saved Recording Is No Longer Available");stopSelfResult(lastStartId);return}
        state.put("phase","transcribing").put("type","transcribing");publish()
        try {
            if(Build.VERSION.SDK_INT>=30)startForeground(NOTIFICATION,notification(),ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE)
            else startForeground(NOTIFICATION,notification())
            wake=getSystemService(PowerManager::class.java).newWakeLock(PowerManager.PARTIAL_WAKE_LOCK,"fastcontrol:voice").apply{acquire()}
            check(recorder.retry(state.getString("id"),state.getString("audioPath"),state.optBoolean("compressed"),server,base))
            observer?.invoke(snapshotJSON)
        } catch (_: Exception) {state.put("phase","error").put("type","error").put("message","Could Not Retry. Your Recording Is Saved");publish();prefs(this).edit().putString(RESULT,snapshotJSON).apply();observer?.invoke(snapshotJSON);stopWhenIdle()}
    }

    private fun onVoiceEvent(event: JSONObject) {
        if (event.optString("id") != state.optString("id") || terminal()) return
        val type = event.optString("type")
        if (state.optString("phase") == "transcribing" && type in setOf("level", "recording")) return
        for (key in event.keys()) state.put(key, event.get(key))
        state.put("phase", if (type == "level") "recording" else type)
        if(type=="transcript"){state.remove("retryable");state.remove("audioPath")}
        if (type == "level") { levels.addLast(event.optDouble("level")); if (levels.size > 24) levels.removeFirst() }
        publish()
        if (terminal()) prefs(this).edit().putString(RESULT, snapshotJSON).apply()
        observer?.invoke(event.toString())
        val second = state.optDouble("seconds").toInt()
        if (type != "level" || second != lastNotificationSecond) {
            lastNotificationSecond = second
            if (!terminal()) notifications.notify(NOTIFICATION, notification())
        }
        stopWhenIdle()
    }

    private fun finishCapture(server: Server, base: String) {
        if (state.optString("phase") !in setOf("preparing", "recording")) return
        if (server.id != state.optString("server") || base.trimEnd('/') !in server.urls.map { it.trimEnd('/') }) return
        state.put("base", base.trimEnd('/'))
        state.put("phase", "transcribing").put("type", "transcribing"); publish()
        notifications.notify(NOTIFICATION, notification())
        recorder.finish(server, base)
        observer?.invoke(JSONObject(state.toString()).toString())
    }

    private fun cancelCapture() {
        if (state.optString("id").isEmpty() || terminal()) return
        recorder.cancel()
        state.put("phase", "cancelled").put("type", "cancelled"); publish()
        prefs(this).edit().putString(RESULT, snapshotJSON).apply()
        observer?.invoke(snapshotJSON)
        stopWhenIdle()
    }

    private fun failAndCancel(message: String) {
        recorder.cancel()
        state.put("phase", "error").put("type", "error").put("message", message); publish()
        prefs(this).edit().putString(RESULT, snapshotJSON).apply()
        observer?.invoke(snapshotJSON)
        stopWhenIdle()
    }

    private fun stopWhenIdle() {
        if (!terminal() || recorder.isBusy) return
        wake?.let { if (it.isHeld) it.release() }; wake = null
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelfResult(lastStartId)
    }

    private fun action(action: String, requestCode: Int): PendingIntent {
        val intent = Intent(this, VoiceCaptureService::class.java).setAction(action)
            .setData(Uri.parse("fastcontrol://voice/" + state.optString("id") + "/" + requestCode))
            .putExtra("id", state.optString("id"))
        return PendingIntent.getService(this, requestCode, intent, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
    }

    private fun notification(): Notification {
        val phase = state.optString("phase")
        val open = Intent(this, MainActivity::class.java).setAction(MainActivity.ACTION_OPEN_THREAD)
            .putExtra(MainActivity.EXTRA_SERVER, state.optString("server"))
            .putExtra(MainActivity.EXTRA_THREAD, state.optString("thread"))
            .putExtra(MainActivity.EXTRA_PROVIDER, state.optString("provider", "codex"))
            .setData(Uri.parse("fastcontrol://voice/" + state.optString("id")))
        val content = PendingIntent.getActivity(this, 103, open, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        val builder = Notification.Builder(this, CHANNEL).setSmallIcon(R.drawable.ic_stat_relay)
            .setContentTitle(if (phase == "transcribing") "Transcribing Voice" else if (phase == "preparing") "Starting Recording" else "Recording Voice")
            .setContentText("Tap To Return To fastcontrol").setContentIntent(content)
            .setOngoing(true).setOnlyAlertOnce(true).setCategory(Notification.CATEGORY_SERVICE)
            .setVisibility(Notification.VISIBILITY_PRIVATE)
        if (phase == "recording") builder.setWhen(System.currentTimeMillis() - (state.optDouble("seconds") * 1000).toLong()).setUsesChronometer(true)
        if (phase == "transcribing") builder.setProgress(0, 0, true)
        if (phase == "recording") builder.addAction(Notification.Action.Builder(null, "Stop And Transcribe", action(FINISH, 104)).build())
        builder.addAction(Notification.Action.Builder(null, "Cancel", action(CANCEL, 105)).build())
        if (Build.VERSION.SDK_INT >= 31) builder.setForegroundServiceBehavior(Notification.FOREGROUND_SERVICE_IMMEDIATE)
        return builder.build()
    }

    override fun onDestroy() {
        if (!terminal() && state.optString("id").isNotEmpty()) {
            state.put("phase", "error").put("type", "error").put("message", "Recording Ended. Try Again")
            publish(); prefs(this).edit().putString(RESULT, snapshotJSON).apply()
            observer?.invoke(snapshotJSON)
        }
        if (instance === this) instance = null
        recorder.cancel()
        wake?.let { if (it.isHeld) it.release() }; wake = null
        super.onDestroy()
    }
}
