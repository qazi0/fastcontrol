package net.siraj.fastcontrol

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.util.Log
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject
import java.util.concurrent.TimeUnit

/**
 * Foreground service that keeps one WebSocket per paired relay and turns
 * `{"t":"event"}` messages into notifications. Runs only while notifications are enabled.
 */
class RelayWatchService : Service() {

    companion object {
        private const val TAG = "RelayWatch"
        private const val FG_ID = 1
        private const val THREAD_ID = 2
        const val CH_CONNECTION = "connection"
        const val CH_TURNS = "turns"
        const val CH_ATTENTION = "attention"

        @Volatile private var instance: RelayWatchService? = null

        val isRunning: Boolean get() = instance != null

        /** Must be called on the main thread. */
        fun reloadIfRunning() {
            instance?.reload()
        }

        private fun tag(serverId: String, threadId: String) = "$serverId/$threadId"

        fun cancelThreadNotification(ctx: Context, serverId: String, threadId: String) {
            ctx.getSystemService(NotificationManager::class.java)
                ?.cancel(tag(serverId, threadId), THREAD_ID)
        }
    }

    private val main = Handler(Looper.getMainLooper())
    private val client = OkHttpClient.Builder()
        .pingInterval(25, TimeUnit.SECONDS)
        .readTimeout(0, TimeUnit.MILLISECONDS)
        .connectTimeout(10, TimeUnit.SECONDS)
        .build()
    private val conns = LinkedHashMap<String, Conn>()
    private lateinit var nm: NotificationManager
    private var netCallback: ConnectivityManager.NetworkCallback? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        instance = this
        nm = getSystemService(NotificationManager::class.java)
        ensureChannels()
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                main.post { conns.values.forEach { it.kick() } }
            }
        }
        try {
            getSystemService(ConnectivityManager::class.java)?.registerDefaultNetworkCallback(cb)
            netCallback = cb
        } catch (e: Exception) {
            Log.w(TAG, "network callback unavailable", e)
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val fg = foregroundNotification()
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(FG_ID, fg, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(FG_ID, fg)
        }
        if (!Store.notificationsEnabled(this)) {
            stopSelf()
            return START_NOT_STICKY
        }
        reload()
        return START_STICKY
    }

    override fun onDestroy() {
        instance = null
        conns.values.forEach { it.close() }
        conns.clear()
        netCallback?.let {
            try {
                getSystemService(ConnectivityManager::class.java)?.unregisterNetworkCallback(it)
            } catch (_: Exception) {
            }
        }
        main.removeCallbacksAndMessages(null)
        super.onDestroy()
    }

    /** Re-reads the server list and reconnects every relay. */
    private fun reload() {
        conns.values.forEach { it.close() }
        conns.clear()
        for (s in Store.servers(this)) {
            if (conns.containsKey(s.id)) continue
            conns[s.id] = Conn(s).also { it.connect() }
        }
        updateForeground()
    }

    private fun ensureChannels() {
        nm.createNotificationChannel(
            NotificationChannel(CH_CONNECTION, "Connection", NotificationManager.IMPORTANCE_LOW).apply {
                setShowBadge(false)
            }
        )
        nm.createNotificationChannel(
            NotificationChannel(CH_TURNS, "Turns", NotificationManager.IMPORTANCE_DEFAULT)
        )
        nm.createNotificationChannel(
            NotificationChannel(CH_ATTENTION, "Attention", NotificationManager.IMPORTANCE_HIGH).apply {
                enableVibration(true)
            }
        )
    }

    private fun openAppIntent(): PendingIntent = PendingIntent.getActivity(
        this, 0,
        Intent(this, MainActivity::class.java).setAction(Intent.ACTION_MAIN)
            .addCategory(Intent.CATEGORY_LAUNCHER),
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
    )

    private fun foregroundNotification(): Notification {
        val total = conns.size
        val online = conns.values.count { it.connected }
        val text = when (total) {
            0 -> "No Macs Paired"
            1 -> "Watching 1 Mac"
            else -> "Watching $total Macs"
        }
        val sub = if (total == 0) null else "$online of $total Connected"
        return Notification.Builder(this, CH_CONNECTION)
            .setSmallIcon(R.drawable.ic_stat_relay)
            .setContentTitle("fastcontrol")
            .setContentText(text)
            .setSubText(sub)
            .setColor(getColor(R.color.accent))
            .setOngoing(true)
            .setShowWhen(false)
            .setContentIntent(openAppIntent())
            .build()
    }

    private fun updateForeground() {
        nm.notify(FG_ID, foregroundNotification())
    }

    private fun canNotify() = Build.VERSION.SDK_INT < 33 ||
        checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED

    private fun onEvent(server: Server, msg: JSONObject) {
        val kind = msg.optString("kind")
        val channel = when (kind) {
            "completed", "failed" -> CH_TURNS
            "attention" -> CH_ATTENTION
            else -> return
        }
        val threadId = msg.optString("id")
        if (threadId.isEmpty()) return
        if (UiState.isShowing(server.id, threadId)) return
        if (!canNotify()) return

        val title = msg.optString("title").ifEmpty { "Codex" }
        val text = msg.optString("text").ifEmpty {
            when (kind) {
                "failed" -> "Turn Failed"
                "attention" -> "Needs Your Attention"
                else -> "Turn Completed"
            }
        }
        val tag = tag(server.id, threadId)
        val open = Intent(this, MainActivity::class.java)
            .setAction(MainActivity.ACTION_OPEN_THREAD)
            .putExtra(MainActivity.EXTRA_SERVER, server.id)
            .putExtra(MainActivity.EXTRA_THREAD, threadId)
        val pi = PendingIntent.getActivity(
            this, tag.hashCode(), open,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val n = Notification.Builder(this, channel)
            .setSmallIcon(R.drawable.ic_stat_relay)
            .setColor(getColor(R.color.accent))
            .setContentTitle(title)
            .setContentText(text)
            .setStyle(Notification.BigTextStyle().bigText(text))
            .setSubText(server.name)
            .setAutoCancel(true)
            .setContentIntent(pi)
            .setCategory(
                if (kind == "attention") Notification.CATEGORY_REMINDER else Notification.CATEGORY_STATUS
            )
            .build()
        nm.notify(tag, THREAD_ID, n)
    }

    /** One relay connection with exponential backoff (1 s up to 30 s). All state lives on main. */
    private inner class Conn(val server: Server) {
        var connected = false
        private var ws: WebSocket? = null
        private var backoffMs = 1000L
        private var closed = false
        private var generation = 0
        private var urlIndex = 0 // which of server.urls to try next
        private val retry = Runnable { connect() }

        fun wsUrl(): String {
            val urls = server.urls
            val base = urls[urlIndex % urls.size].trimEnd('/')
            val ws = when {
                base.startsWith("https://") -> "wss://" + base.removePrefix("https://")
                base.startsWith("http://") -> "ws://" + base.removePrefix("http://")
                base.startsWith("ws://") || base.startsWith("wss://") -> base
                else -> "ws://$base"
            }
            // events=1: the relay sends only events, never the large thread lists.
            return "$ws/ws?events=1&t=" + android.net.Uri.encode(server.token)
        }

        fun connect() {
            if (closed) return
            main.removeCallbacks(retry)
            ws?.cancel()
            val gen = ++generation
            val req = try {
                Request.Builder().url(wsUrl()).build()
            } catch (e: IllegalArgumentException) {
                Log.w(TAG, "bad relay url for ${server.name}")
                return
            }
            Log.i(TAG, "connecting to ${server.name}")
            ws = client.newWebSocket(req, object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) {
                    webSocket.send("""{"op":"events","on":true}""")
                    main.post {
                        if (gen != generation) return@post
                        Log.i(TAG, "connected to ${server.name}")
                        connected = true
                        backoffMs = 1000L
                        updateForeground()
                    }
                }

                override fun onMessage(webSocket: WebSocket, text: String) {
                    // Cheap check first; only events are worth parsing.
                    if (!text.contains("\"t\":\"event\"") && !text.contains("\"t\":\"hello\"")) return
                    val msg = try { JSONObject(text) } catch (_: Exception) { return }
                    val t = msg.optString("t")
                    if (t == "hello") Log.i(TAG, "hello from ${server.name}")
                    if (t != "event") return
                    main.post { if (gen == generation) onEvent(server, msg) }
                }

                override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                    webSocket.close(1000, null)
                }

                override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                    main.post { if (gen == generation) dropped("closed $code") }
                }

                override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                    main.post { if (gen == generation) dropped(t.javaClass.simpleName) }
                }
            })
        }

        private fun dropped(why: String) {
            if (closed) return
            Log.i(TAG, "lost ${server.name} ($why), retry in ${backoffMs}ms")
            val was = connected
            connected = false
            ws = null
            if (was) updateForeground()
            // Never connected on this address: try the next one (Wi-Fi, Tailscale) right away.
            if (!was && server.urls.size > 1 && ++urlIndex % server.urls.size != 0) {
                main.post(retry)
                return
            }
            main.postDelayed(retry, backoffMs)
            backoffMs = (backoffMs * 2).coerceAtMost(30_000L)
        }

        /** Network came back: retry a dropped relay immediately. */
        fun kick() {
            // Only a relay waiting on its backoff timer has ws == null.
            if (closed || ws != null) return
            backoffMs = 1000L
            connect()
        }

        fun close() {
            closed = true
            generation++
            main.removeCallbacks(retry)
            ws?.close(1000, null)
            ws = null
            connected = false
        }
    }
}
