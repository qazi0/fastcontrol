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
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.SystemClock
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

        internal const val STAGGER_MS = 300L
        internal const val CONNECT_TIMEOUT_SEC = 5L
        internal const val RACE_MIN_DEADLINE_MS = 5_500L
        internal const val RACE_MAX_DEADLINE_MS = 10_000L
        internal const val PROBE_TIMEOUT_MS = 3_500L

        @Volatile private var instance: RelayWatchService? = null

        val isRunning: Boolean get() = instance != null

        /** Must be called on the main thread. */
        fun reloadIfRunning() {
            instance?.reload()
        }

        /** Nudges running watch connections after a network transition or mDNS discovery. */
        fun nudgeIfRunning(serverId: String? = null, preferredUrl: String? = null) {
            val svc = instance ?: return
            svc.main.post { svc.nudge(serverId, preferredUrl) }
        }

        internal data class ThreadNotificationSpec(
            val channel: String,
            val serverId: String,
            val threadId: String,
            val provider: String,
            val tag: String,
            val title: String,
            val text: String,
            val subText: String,
            val isAttention: Boolean,
        )

        internal fun tag(serverId: String, threadId: String, provider: String) =
            "$serverId/${Providers.normalize(provider)}/$threadId"

        internal fun buildNotificationSpec(
            serverName: String,
            serverId: String,
            kind: String,
            threadId: String,
            rawProvider: String?,
            rawTitle: String?,
            rawText: String?,
            prefs: NotificationPrefs = NotificationPrefs(),
        ): ThreadNotificationSpec? {
            val normalizedKind = kind.trim().lowercase()
            if (!prefs.shouldNotify(normalizedKind)) return null
            val channel = when (normalizedKind) {
                "completed", "failed" -> CH_TURNS
                "attention" -> CH_ATTENTION
                else -> return null
            }
            if (threadId.isEmpty()) return null
            val provider = Providers.normalize(rawProvider)
            val cleanTitle = rawTitle?.trim().orEmpty()
            val cleanText = rawText?.trim().orEmpty()
            val title = cleanTitle.ifEmpty { Providers.fallbackNotificationTitle(provider, normalizedKind) }
            val text = cleanText.ifEmpty { Providers.fallbackNotificationText(normalizedKind, hasExplicitTitle = cleanTitle.isNotEmpty()) }
            return ThreadNotificationSpec(
                channel = channel,
                serverId = serverId,
                threadId = threadId,
                provider = provider,
                tag = tag(serverId, threadId, provider),
                title = title,
                text = text,
                subText = Providers.notificationSubText(serverName, provider),
                isAttention = normalizedKind == "attention",
            )
        }

        internal fun resolveEventNotification(
            ctx: Context,
            server: Server,
            msg: JSONObject,
        ): ThreadNotificationSpec? {
            val normalizedKind = msg.optString("kind").trim().lowercase()
            if (!Store.shouldNotify(ctx, normalizedKind)) return null
            return buildNotificationSpec(
                serverName = server.name,
                serverId = server.id,
                kind = normalizedKind,
                threadId = msg.optString("id"),
                rawProvider = msg.optString("provider"),
                rawTitle = msg.optString("title"),
                rawText = msg.optString("text"),
            )
        }

        fun cancelThreadNotification(ctx: Context, serverId: String, threadId: String, provider: String) {
            ctx.getSystemService(NotificationManager::class.java)
                ?.cancel(tag(serverId, threadId, provider), THREAD_ID)
        }

        /**
         * Orders candidate relay URLs for staggered happy-eyeballs racing while enforcing
         * HTTPS/pin no-downgrade rules and avoiding duplicates.
         */
        internal fun buildCandidateUrls(
            server: Server,
            preferredUrl: String? = null,
            lastWorkingUrl: String? = null,
        ): List<String> {
            val ordered = LinkedHashSet<String>()
            fun addIfAllowed(raw: String?) {
                if (raw.isNullOrBlank()) return
                val normalized = Store.normalizeOrigin(raw, allowLoopback = true) ?: return
                if (!server.allowsUrl(normalized)) return
                ordered += normalized
            }
            addIfAllowed(preferredUrl)
            addIfAllowed(lastWorkingUrl)
            addIfAllowed(server.url)
            for (alt in server.alts) addIfAllowed(alt)
            return ordered.toList()
        }

        internal fun extractAddresses(msg: JSONObject): List<String> {
            val arr = msg.optJSONArray("addrs") ?: return emptyList()
            val out = ArrayList<String>(arr.length())
            for (i in 0 until arr.length()) {
                val value = arr.optString(i).trim()
                if (value.isNotEmpty()) out += value
            }
            return out
        }

        internal fun toWebSocketUrl(base: String, token: String): String {
            val trimmed = base.trimEnd('/')
            val ws = when {
                trimmed.startsWith("https://", ignoreCase = true) -> "wss://" + trimmed.substring(8)
                trimmed.startsWith("http://", ignoreCase = true) -> "ws://" + trimmed.substring(7)
                trimmed.startsWith("ws://", ignoreCase = true) || trimmed.startsWith("wss://", ignoreCase = true) -> trimmed
                else -> "ws://$trimmed"
            }
            // events=1: the relay sends only events, never the large thread lists.
            return "$ws/ws?events=1&t=" + android.net.Uri.encode(token)
        }
    }

    private val main = Handler(Looper.getMainLooper())
    private val client = OkHttpClient.Builder()
        .pingInterval(25, TimeUnit.SECONDS)
        .readTimeout(0, TimeUnit.MILLISECONDS)
        .connectTimeout(CONNECT_TIMEOUT_SEC, TimeUnit.SECONDS)
        .build()
    private val conns = LinkedHashMap<String, Conn>()
    private lateinit var nm: NotificationManager
    private var networkMonitor: NetworkMonitor? = null

    private val discoveryListener: (DiscoveredRelay) -> Unit = { discovery ->
        main.post { onRelayDiscovered(discovery) }
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        instance = this
        nm = getSystemService(NotificationManager::class.java)
        ensureChannels()

        networkMonitor = NetworkMonitor(this, tag = TAG) { event ->
            onNetworkEvent(event)
        }.also { it.start() }

        RelayDiscovery.acquire(this, "watch", discoveryListener)
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
        RelayDiscovery.release("watch")
        networkMonitor?.stop()
        networkMonitor = null
        conns.values.forEach { it.close() }
        conns.clear()
        client.connectionPool.evictAll()
        main.removeCallbacksAndMessages(null)
        super.onDestroy()
    }

    private fun onNetworkEvent(event: NetworkEvent) {
        Log.i(TAG, "network change (${event.reason}, online=${event.online}, wifi=${event.wifi}, vpn=${event.vpn})")
        client.connectionPool.evictAll()
        if (event.lanChanged && event.online) {
            RelayDiscovery.restart(this)
        }
        if (event.online) {
            conns.values.forEach { it.onNetworkChange() }
        }
    }

    private fun onRelayDiscovered(discovery: DiscoveredRelay) {
        val fresh = Store.servers(this).firstOrNull { it.id == discovery.serverId } ?: return
        val conn = conns[discovery.serverId] ?: return
        conn.onDiscovered(fresh, discovery.url)
    }

    private fun nudge(serverId: String?, preferredUrl: String?) {
        if (serverId != null) {
            val fresh = Store.servers(this).firstOrNull { it.id == serverId }
            val conn = conns[serverId] ?: return
            if (fresh != null && preferredUrl != null) {
                conn.onDiscovered(fresh, preferredUrl)
            } else {
                if (fresh != null) conn.updateServer(fresh)
                conn.onNetworkChange()
            }
            return
        }
        conns.values.forEach { it.onNetworkChange() }
    }

    /** Re-reads the server list and reconciles watch connections without breaking healthy ones. */
    private fun reload() {
        val latest = Store.servers(this)
        val latestById = LinkedHashMap<String, Server>(latest.size)
        for (s in latest) {
            if (!latestById.containsKey(s.id)) latestById[s.id] = s
        }

        val removed = conns.keys.filter { it !in latestById }
        for (id in removed) {
            conns.remove(id)?.close()
        }

        for ((id, s) in latestById) {
            val existing = conns[id]
            if (existing == null) {
                conns[id] = Conn(s).also { it.connect() }
            } else if (
                existing.server.url != s.url ||
                existing.server.token != s.token ||
                existing.server.pin != s.pin
            ) {
                existing.close()
                conns[id] = Conn(s).also { it.connect() }
            } else {
                existing.updateServer(s)
            }
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
        val text = when {
            total == 0 -> "No Computers Paired"
            online == 0 -> if (total == 1) "1 Computer Offline" else "$total Computers Offline"
            online == total -> if (total == 1) "1 Computer Connected" else "$total Computers Connected"
            else -> "$online Of $total Computers Connected"
        }
        return Notification.Builder(this, CH_CONNECTION)
            .setSmallIcon(R.drawable.ic_stat_relay)
            .setContentTitle("fastcontrol")
            .setContentText(text)
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
        val spec = resolveEventNotification(this, server, msg) ?: return
        if (UiState.isShowing(spec.serverId, spec.threadId, spec.provider)) return
        if (!canNotify()) return

        val dataUri = android.net.Uri.Builder()
            .scheme("fastcontrol")
            .authority("thread")
            .appendPath(spec.serverId)
            .appendPath(spec.provider)
            .appendPath(spec.threadId)
            .build()
        val open = MainActivity.openThreadIntent(
            context = this,
            serverId = spec.serverId,
            threadId = spec.threadId,
            provider = spec.provider,
            dataUri = dataUri,
        )
        val pi = PendingIntent.getActivity(
            this, spec.tag.hashCode(), open,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val n = Notification.Builder(this, spec.channel)
            .setSmallIcon(R.drawable.ic_stat_relay)
            .setColor(getColor(R.color.accent))
            .setContentTitle(spec.title)
            .setContentText(spec.text)
            .setStyle(Notification.BigTextStyle().bigText(spec.text))
            .setSubText(spec.subText)
            .setAutoCancel(true)
            .setContentIntent(pi)
            .setCategory(
                if (spec.isAttention) Notification.CATEGORY_REMINDER else Notification.CATEGORY_STATUS
            )
            .build()
        nm.notify(spec.tag, THREAD_ID, n)
    }

    /**
     * One relay connection with staggered happy-eyeballs URL racing and exponential backoff
     * (1 s up to 30 s). All mutable state lives exclusively on the main thread.
     */
    private inner class Conn(var server: Server) {
        var connected = false
        private var ws: WebSocket? = null
        private var activeBase: String? = null
        private var lastWorkingUrl: String? = null
        private var heartbeatSupported = false
        @Volatile private var probing = false
        private var backoffMs = 1000L
        private var closed = false
        private var generation = 0

        private var raceCandidates: List<String> = emptyList()
        private var nextLaunchIndex = 0
        private var failedCandidates = 0
        private var raceStartedAtMs = 0L
        private val racingSockets = LinkedHashMap<WebSocket, String>()

        private val retry = Runnable { connect() }
        private val stagger = Runnable { launchNextCandidate() }
        private val raceDeadline = Runnable { onRaceTimedOut() }
        private val probeTimeout = Runnable { onProbeTimedOut() }

        fun connect(preferredUrl: String? = null) {
            if (closed) return
            cancelTimers()
            val wasConnected = connected
            cancelAllSockets()
            if (wasConnected) updateForeground()

            generation++
            raceCandidates = buildCandidateUrls(server, preferredUrl, lastWorkingUrl)
            if (raceCandidates.isEmpty()) {
                Log.w(TAG, "no valid relay url for ${server.name}")
                return
            }
            nextLaunchIndex = 0
            failedCandidates = 0
            raceStartedAtMs = SystemClock.elapsedRealtime()

            val deadlineMs = (raceCandidates.size * STAGGER_MS + 4_500L)
                .coerceIn(RACE_MIN_DEADLINE_MS, RACE_MAX_DEADLINE_MS)
            main.postDelayed(raceDeadline, deadlineMs)
            Log.i(TAG, "racing ${raceCandidates.size} route(s) for ${server.name}")
            launchNextCandidate()
        }

        private fun launchNextCandidate() {
            if (closed || ws != null) return
            if (nextLaunchIndex >= raceCandidates.size) return
            val base = raceCandidates[nextLaunchIndex++]
            val gen = generation

            val req = try {
                Request.Builder().url(toWebSocketUrl(base, server.token)).build()
            } catch (_: IllegalArgumentException) {
                Log.w(TAG, "bad relay url for ${server.name}: $base")
                failedCandidates++
                if (failedCandidates >= raceCandidates.size) onRaceFailed("bad url")
                else launchNextCandidate()
                return
            }

            if (nextLaunchIndex < raceCandidates.size) {
                main.removeCallbacks(stagger)
                main.postDelayed(stagger, STAGGER_MS)
            }

            val socket = try {
                RelayTLS.client(server, client).newWebSocket(req, object : WebSocketListener() {
                    override fun onOpen(webSocket: WebSocket, response: Response) {
                        main.post {
                            if (gen != generation || closed || ws != null) {
                                webSocket.cancel()
                                return@post
                            }
                            ws = webSocket
                            activeBase = base
                            lastWorkingUrl = base
                            connected = true
                            backoffMs = 1000L
                            heartbeatSupported = false
                            clearProbe()
                            main.removeCallbacks(stagger)
                            main.removeCallbacks(raceDeadline)
                            cancelLosers(winner = webSocket)
                            webSocket.send("""{"op":"events","on":true}""")
                            Log.i(TAG, "connected to ${server.name} via $base")
                            updateForeground()
                        }
                    }

                    override fun onMessage(webSocket: WebSocket, text: String) {
                        if (probing) {
                            main.post { if (gen == generation && ws === webSocket) clearProbe() }
                        }
                        val interested = text.contains("\"t\":\"event\"") ||
                            text.contains("\"t\":\"hello\"") ||
                            text.contains("\"t\":\"network\"") ||
                            text.contains("\"t\":\"pong\"")
                        if (!interested) return
                        val msg = try { JSONObject(text) } catch (_: Exception) { return }
                        when (msg.optString("t")) {
                            "pong" -> main.post {
                                if (gen == generation && ws === webSocket) clearProbe()
                            }
                            "hello" -> {
                                Log.i(TAG, "hello from ${server.name}")
                                val hb = msg.optBoolean("heartbeat", false)
                                val addrs = extractAddresses(msg)
                                val host = msg.optString("host")
                                main.post {
                                    if (gen == generation && ws === webSocket) {
                                        clearProbe()
                                        heartbeatSupported = hb
                                        onAddressesUpdated(addrs, host)
                                    }
                                }
                            }
                            "network" -> {
                                val addrs = extractAddresses(msg)
                                main.post {
                                    if (gen == generation && ws === webSocket) {
                                        clearProbe()
                                        onAddressesUpdated(addrs, "")
                                    }
                                }
                            }
                            "event" -> main.post {
                                if (gen == generation && ws === webSocket) {
                                    clearProbe()
                                    onEvent(server, msg)
                                }
                            }
                        }
                    }

                    override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                        webSocket.close(1000, null)
                    }

                    override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                        main.post { handleSocketEnded(gen, webSocket, "closed $code") }
                    }

                    override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                        response?.close()
                        main.post { handleSocketEnded(gen, webSocket, t.javaClass.simpleName) }
                    }
                })
            } catch (e: Exception) {
                Log.w(TAG, "websocket start failed for ${server.name} via $base (${e.javaClass.simpleName})")
                failedCandidates++
                if (failedCandidates >= raceCandidates.size) onRaceFailed(e.javaClass.simpleName)
                else {
                    main.removeCallbacks(stagger)
                    launchNextCandidate()
                }
                return
            }
            racingSockets[socket] = base
        }

        private fun onAddressesUpdated(addrs: List<String>, host: String) {
            if (addrs.isEmpty()) return
            val result = Store.mergeServerUrls(this@RelayWatchService, server.id, addrs) ?: return
            server = result.server
            if (result.changed) {
                val preferred = result.preferredUrl ?: activeBase ?: result.server.url
                Log.i(TAG, "updated routes for ${server.name} (${result.server.alts.size} alternates)")
                RelayDiscovery.notifyWatchAddresses(result.server, preferred, host)
            }
        }

        private fun handleSocketEnded(gen: Int, webSocket: WebSocket, why: String) {
            if (gen != generation || closed) return
            if (ws === webSocket) {
                clearProbe()
                ws = null
                activeBase = null
                heartbeatSupported = false
                val was = connected
                connected = false
                if (was) updateForeground()
                scheduleBackoffRetry(why)
                return
            }
            if (racingSockets.remove(webSocket) == null) return
            failedCandidates++
            if (ws == null) {
                if (failedCandidates >= raceCandidates.size) {
                    onRaceFailed(why)
                } else if (nextLaunchIndex < raceCandidates.size) {
                    // Fast-advance to the next route instead of waiting out the stagger window.
                    main.removeCallbacks(stagger)
                    launchNextCandidate()
                }
            }
        }

        private fun onRaceTimedOut() {
            if (closed || ws != null) return
            generation++
            cancelTimers()
            cancelRacingSockets()
            scheduleBackoffRetry("timeout")
        }

        private fun onRaceFailed(why: String) {
            if (closed || ws != null) return
            cancelTimers()
            cancelRacingSockets()
            scheduleBackoffRetry(why)
        }

        private fun scheduleBackoffRetry(why: String) {
            Log.i(TAG, "lost ${server.name} ($why), retry in ${backoffMs}ms")
            main.removeCallbacks(retry)
            main.postDelayed(retry, backoffMs)
            backoffMs = (backoffMs * 2).coerceAtMost(30_000L)
        }

        private fun clearProbe() {
            probing = false
            main.removeCallbacks(probeTimeout)
        }

        private fun onProbeTimedOut() {
            if (closed || !probing || ws == null) return
            Log.i(TAG, "probe timed out for ${server.name}, reconnecting")
            clearProbe()
            backoffMs = 1000L
            connect()
        }

        /** Network changed: probe live sockets or immediately race fresh candidate routes. */
        fun onNetworkChange() {
            if (closed) return
            val active = ws
            if (connected && active != null) {
                if (!heartbeatSupported) return
                if (probing) return
                probing = true
                main.removeCallbacks(probeTimeout)
                main.postDelayed(probeTimeout, PROBE_TIMEOUT_MS)
                val sent = try { active.send("""{"op":"ping"}""") } catch (_: Exception) { false }
                if (!sent) {
                    clearProbe()
                    backoffMs = 1000L
                    connect()
                }
                return
            }
            // Avoid restarting an identical race that just launched moments ago.
            if (racingSockets.isNotEmpty() && SystemClock.elapsedRealtime() - raceStartedAtMs < 500L) return
            backoffMs = 1000L
            connect()
        }

        /** mDNS resolved a matching relay endpoint on LAN. */
        fun onDiscovered(updatedServer: Server, preferredUrl: String) {
            server = updatedServer
            if (closed) return
            if (connected && ws != null) return
            if (preferredUrl in racingSockets.values) return
            backoffMs = 1000L
            connect(preferredUrl = preferredUrl)
        }

        /** Non-disruptive server metadata/alts update from Store reload. */
        fun updateServer(updatedServer: Server) {
            val previousUrls = server.urls.toSet()
            server = updatedServer
            if (closed || (connected && ws != null)) return
            val newlyAdded = updatedServer.urls.firstOrNull { it !in previousUrls }
            if (newlyAdded != null && newlyAdded !in racingSockets.values) {
                backoffMs = 1000L
                connect(preferredUrl = newlyAdded)
            }
        }

        private fun cancelLosers(winner: WebSocket) {
            val losers = racingSockets.keys.filter { it !== winner }
            racingSockets.clear()
            for (loser in losers) loser.cancel()
        }

        private fun cancelRacingSockets() {
            val active = racingSockets.keys.toList()
            racingSockets.clear()
            for (socket in active) socket.cancel()
        }

        private fun cancelAllSockets() {
            clearProbe()
            val active = ws
            ws = null
            activeBase = null
            heartbeatSupported = false
            connected = false
            active?.cancel()
            cancelRacingSockets()
        }

        private fun cancelTimers() {
            main.removeCallbacks(retry)
            main.removeCallbacks(stagger)
            main.removeCallbacks(raceDeadline)
            main.removeCallbacks(probeTimeout)
            probing = false
        }

        fun close() {
            closed = true
            generation++
            cancelTimers()
            val active = ws
            ws = null
            activeBase = null
            heartbeatSupported = false
            connected = false
            active?.close(1000, null)
            cancelRacingSockets()
        }
    }
}
