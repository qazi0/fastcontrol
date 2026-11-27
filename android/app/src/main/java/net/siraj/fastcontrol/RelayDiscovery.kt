package net.siraj.fastcontrol

import android.content.Context
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.util.Log
import org.json.JSONArray
import org.json.JSONObject
import java.net.Inet4Address
import java.net.Inet6Address
import java.net.InetAddress
import java.util.concurrent.Executor

data class DiscoveredRelay(
    val serverId: String,
    val url: String,
    val alts: List<String>,
    val host: String,
    val tls: Boolean,
    val source: String = "mdns",
) {
    fun toJson(): JSONObject = JSONObject()
        .put("id", serverId)
        .put("serverId", serverId)
        .put("url", url)
        .put("alts", JSONArray(alts))
        .put("urls", JSONArray(alts))
        .put("host", host)
        .put("tls", tls)
        .put("source", source)
}

internal data class ResolvedNsdService(
    val serviceName: String,
    val fp: String,
    val host: String,
    val tls: Boolean,
    val version: String,
    val port: Int,
    val addresses: List<InetAddress>,
)

/**
 * Shared NsdManager mDNS discovery manager for `_fastcontrol._tcp.` services.
 *
 * Ref-counted by named client keys (for example "main" and "watch") so MainActivity and
 * RelayWatchService share a single system discovery session without listener leaks or
 * IllegalArgumentException crashes on double-stop.
 */
object RelayDiscovery {
    private const val TAG = "RelayDiscovery"
    const val SERVICE_TYPE = "_fastcontrol._tcp."
    private const val RESOLVE_TIMEOUT_MS = 5_000L
    private const val STOP_FALLBACK_MS = 1_500L
    private const val NOTIFY_COOLDOWN_MS = 2_000L
    private const val RESTART_DEBOUNCE_MS = 400L

    private val FP_REGEX = Regex("^[0-9a-f]{12}$")
    private val HOST_LABEL_REGEX = Regex("^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\\.local)?$")

    private enum class DiscoveryState { STOPPED, STARTING, RUNNING, STOPPING }

    private val main: Handler by lazy { Handler(Looper.getMainLooper()) }
    private val mainExecutor = Executor { runnable -> main.post(runnable) }

    private var appContext: Context? = null
    private var state = DiscoveryState.STOPPED
    private var wantRunning = false
    private var pendingRestart = false
    private var activeDiscoveryListener: NsdManager.DiscoveryListener? = null

    private val clients = LinkedHashMap<String, (DiscoveredRelay) -> Unit>()
    private val resolvedServices = LinkedHashMap<String, ResolvedNsdService>()
    private val discoveredByServer = LinkedHashMap<String, DiscoveredRelay>()
    private val lastNotifiedAt = HashMap<String, Long>()

    // API 34+ ServiceInfoCallback tracking (keyed by serviceName).
    private val infoCallbacks = LinkedHashMap<String, Any>()

    // API < 34 serialized resolve queue (NsdManager rejects concurrent resolveService calls).
    private val resolveQueue = ArrayDeque<NsdServiceInfo>()
    private val queuedServiceNames = HashSet<String>()
    private var resolvingName: String? = null
    private var resolveGen = 0

    private val resolveTimeout = Runnable {
        if (resolvingName != null) {
            Log.w(TAG, "resolve timed out for $resolvingName")
            resolvingName = null
            resolveGen++
            pumpResolveQueue()
        }
    }

    private val stopFallback = Runnable {
        if (state == DiscoveryState.STOPPING) {
            activeDiscoveryListener = null
            state = DiscoveryState.STOPPED
            if (wantRunning || pendingRestart) {
                pendingRestart = false
                startDiscoveryLocked()
            }
        }
    }

    private val restartTask = Runnable {
        if (wantRunning) restartNowLocked()
    }

    fun acquire(ctx: Context, clientKey: String, listener: (DiscoveredRelay) -> Unit) {
        val app = ctx.applicationContext
        main.post {
            appContext = app
            clients[clientKey] = listener
            wantRunning = true
            ensureStartedLocked()
        }
    }

    fun release(clientKey: String) {
        main.post {
            clients.remove(clientKey)
            if (clients.isEmpty()) {
                wantRunning = false
                pendingRestart = false
                main.removeCallbacks(restartTask)
                stopDiscoveryLocked()
            }
        }
    }

    /** Restarts mDNS browsing (debounced) when LAN interfaces change. */
    fun restart(ctx: Context) {
        val app = ctx.applicationContext
        main.post {
            appContext = app
            if (!wantRunning) return@post
            main.removeCallbacks(restartTask)
            main.postDelayed(restartTask, RESTART_DEBOUNCE_MS)
        }
    }

    /** Re-matches already resolved services after WebView adds or edits saved servers. */
    fun rematchSavedServers(ctx: Context) {
        val app = ctx.applicationContext
        main.post {
            appContext = app
            val savedIds = Store.servers(app).map { it.id }.toSet()
            synchronized(discoveredByServer) {
                discoveredByServer.keys.retainAll(savedIds)
            }
            for (resolved in resolvedServices.values.toList()) {
                applyResolvedServiceLocked(app, resolved, onlyIfChangedOrNew = true)
            }
        }
    }

    /**
     * Propagates updated addresses learned from `hello.addrs` or `network` WebSocket frames in
     * RelayWatchService so a foregrounded MainActivity can update WebView too.
     */
    fun notifyWatchAddresses(server: Server, preferredUrl: String, host: String) {
        main.post {
            val discovery = DiscoveredRelay(
                serverId = server.id,
                url = preferredUrl,
                alts = server.alts,
                host = host.ifEmpty { server.name },
                tls = server.requiresHttps,
                source = "watch",
            )
            synchronized(discoveredByServer) {
                discoveredByServer[server.id] = discovery
            }
            dispatchLocked(discovery, force = true, skipClient = "watch")
        }
    }

    fun snapshotForSavedServers(ctx: Context): List<DiscoveredRelay> {
        val savedIds = Store.servers(ctx).map { it.id }.toSet()
        synchronized(discoveredByServer) {
            return discoveredByServer.values.filter { it.serverId in savedIds }
        }
    }

    fun snapshotJson(ctx: Context): String {
        val arr = JSONArray()
        for (item in snapshotForSavedServers(ctx)) {
            arr.put(item.toJson())
        }
        return arr.toString()
    }

    private fun nsdManager(): NsdManager? =
        appContext?.getSystemService(Context.NSD_SERVICE) as? NsdManager

    private fun ensureStartedLocked() {
        when (state) {
            DiscoveryState.STOPPED -> startDiscoveryLocked()
            DiscoveryState.STOPPING -> pendingRestart = true
            DiscoveryState.STARTING, DiscoveryState.RUNNING -> Unit
        }
    }

    private fun restartNowLocked() {
        when (state) {
            DiscoveryState.RUNNING -> {
                pendingRestart = true
                stopDiscoveryLocked()
                if (state == DiscoveryState.STOPPED && wantRunning) {
                    pendingRestart = false
                    startDiscoveryLocked()
                }
            }
            DiscoveryState.STARTING -> pendingRestart = true
            DiscoveryState.STOPPING -> pendingRestart = true
            DiscoveryState.STOPPED -> startDiscoveryLocked()
        }
    }

    private fun startDiscoveryLocked() {
        if (!wantRunning || state != DiscoveryState.STOPPED) return
        val nsd = nsdManager() ?: return
        state = DiscoveryState.STARTING

        val listener = object : NsdManager.DiscoveryListener {
            override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) {
                main.post {
                    if (activeDiscoveryListener !== this) return@post
                    Log.w(TAG, "start discovery failed ($errorCode)")
                    activeDiscoveryListener = null
                    state = DiscoveryState.STOPPED
                }
            }

            override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) {
                main.post {
                    if (activeDiscoveryListener !== this) return@post
                    Log.w(TAG, "stop discovery failed ($errorCode)")
                    main.removeCallbacks(stopFallback)
                    activeDiscoveryListener = null
                    state = DiscoveryState.STOPPED
                    if (wantRunning && pendingRestart) {
                        pendingRestart = false
                        startDiscoveryLocked()
                    }
                }
            }

            override fun onDiscoveryStarted(serviceType: String) {
                main.post {
                    if (activeDiscoveryListener !== this) return@post
                    state = DiscoveryState.RUNNING
                    Log.i(TAG, "discovery started for $SERVICE_TYPE")
                    if (!wantRunning) {
                        stopDiscoveryLocked()
                    } else if (pendingRestart) {
                        stopDiscoveryLocked()
                        if (state == DiscoveryState.STOPPED && wantRunning) {
                            pendingRestart = false
                            startDiscoveryLocked()
                        }
                    }
                }
            }

            override fun onDiscoveryStopped(serviceType: String) {
                main.post {
                    if (activeDiscoveryListener !== this) return@post
                    main.removeCallbacks(stopFallback)
                    activeDiscoveryListener = null
                    state = DiscoveryState.STOPPED
                    Log.i(TAG, "discovery stopped")
                    if (wantRunning && pendingRestart) {
                        pendingRestart = false
                        startDiscoveryLocked()
                    }
                }
            }

            override fun onServiceFound(serviceInfo: NsdServiceInfo) {
                main.post {
                    if (activeDiscoveryListener !== this || !wantRunning) return@post
                    val type = serviceInfo.serviceType.orEmpty()
                    if (!type.contains("_fastcontrol._tcp")) return@post
                    beginResolveLocked(serviceInfo)
                }
            }

            override fun onServiceLost(serviceInfo: NsdServiceInfo) {
                main.post {
                    if (activeDiscoveryListener !== this) return@post
                    val name = serviceInfo.serviceName.orEmpty()
                    if (name.isEmpty()) return@post
                    resolvedServices.remove(name)
                    unregisterInfoCallbackLocked(name)
                }
            }
        }

        activeDiscoveryListener = listener
        try {
            nsd.discoverServices(SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, listener)
        } catch (e: Exception) {
            Log.w(TAG, "discoverServices failed", e)
            activeDiscoveryListener = null
            state = DiscoveryState.STOPPED
        }
    }

    private fun stopDiscoveryLocked() {
        clearResolutionStateLocked()
        val listener = activeDiscoveryListener ?: run {
            state = DiscoveryState.STOPPED
            return
        }
        if (state == DiscoveryState.STOPPED || state == DiscoveryState.STOPPING) return
        if (state == DiscoveryState.STARTING) {
            // Wait for onDiscoveryStarted before stopping so Android does not throw.
            return
        }
        state = DiscoveryState.STOPPING
        val nsd = nsdManager()
        if (nsd == null) {
            activeDiscoveryListener = null
            state = DiscoveryState.STOPPED
            return
        }
        main.removeCallbacks(stopFallback)
        main.postDelayed(stopFallback, STOP_FALLBACK_MS)
        try {
            nsd.stopServiceDiscovery(listener)
        } catch (e: IllegalArgumentException) {
            Log.w(TAG, "discovery listener already stopped", e)
            main.removeCallbacks(stopFallback)
            activeDiscoveryListener = null
            state = DiscoveryState.STOPPED
        } catch (e: Exception) {
            Log.w(TAG, "stopServiceDiscovery failed", e)
            main.removeCallbacks(stopFallback)
            activeDiscoveryListener = null
            state = DiscoveryState.STOPPED
        }
    }

    private fun clearResolutionStateLocked() {
        main.removeCallbacks(resolveTimeout)
        resolveQueue.clear()
        queuedServiceNames.clear()
        resolvingName = null
        resolveGen++
        val nsd = nsdManager()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE && nsd != null) {
            for (cb in infoCallbacks.values.toList()) {
                try {
                    nsd.unregisterServiceInfoCallback(cb as NsdManager.ServiceInfoCallback)
                } catch (_: Exception) {
                }
            }
        }
        infoCallbacks.clear()
    }

    private fun unregisterInfoCallbackLocked(serviceName: String) {
        val cb = infoCallbacks.remove(serviceName) ?: return
        val nsd = nsdManager() ?: return
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            try {
                nsd.unregisterServiceInfoCallback(cb as NsdManager.ServiceInfoCallback)
            } catch (_: Exception) {
            }
        }
    }

    private fun beginResolveLocked(serviceInfo: NsdServiceInfo) {
        val name = serviceInfo.serviceName.orEmpty()
        if (name.isEmpty()) return
        val nsd = nsdManager() ?: return

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            unregisterInfoCallbackLocked(name)
            val cb = object : NsdManager.ServiceInfoCallback {
                override fun onServiceInfoCallbackRegistrationFailed(errorCode: Int) {
                    Log.w(TAG, "service info callback registration failed for $name ($errorCode)")
                    infoCallbacks.remove(name)
                }

                override fun onServiceUpdated(updated: NsdServiceInfo) {
                    if (infoCallbacks[name] !== this || !wantRunning) return
                    handleResolvedInfoLocked(updated)
                }

                override fun onServiceLost() {
                    if (infoCallbacks[name] !== this) return
                    resolvedServices.remove(name)
                    unregisterInfoCallbackLocked(name)
                }

                override fun onServiceInfoCallbackUnregistered() {
                    if (infoCallbacks[name] === this) {
                        infoCallbacks.remove(name)
                    }
                }
            }
            infoCallbacks[name] = cb
            try {
                nsd.registerServiceInfoCallback(serviceInfo, mainExecutor, cb)
            } catch (e: Exception) {
                Log.w(TAG, "registerServiceInfoCallback failed for $name", e)
                infoCallbacks.remove(name)
            }
            return
        }

        // API 26..33: serialize resolveService calls to avoid FAILURE_ALREADY_ACTIVE.
        if (resolvingName == name || name in queuedServiceNames) return
        queuedServiceNames += name
        resolveQueue.addLast(serviceInfo)
        pumpResolveQueue()
    }

    private fun pumpResolveQueue() {
        if (!wantRunning || resolvingName != null) return
        val next = resolveQueue.removeFirstOrNull() ?: return
        val name = next.serviceName.orEmpty()
        queuedServiceNames.remove(name)
        val nsd = nsdManager() ?: return

        val gen = ++resolveGen
        resolvingName = name
        main.removeCallbacks(resolveTimeout)
        main.postDelayed(resolveTimeout, RESOLVE_TIMEOUT_MS)

        @Suppress("DEPRECATION")
        val listener = object : NsdManager.ResolveListener {
            override fun onResolveFailed(serviceInfo: NsdServiceInfo, errorCode: Int) {
                main.post {
                    if (gen != resolveGen) return@post
                    main.removeCallbacks(resolveTimeout)
                    resolvingName = null
                    Log.w(TAG, "resolve failed for $name ($errorCode)")
                    if (errorCode == NsdManager.FAILURE_ALREADY_ACTIVE && wantRunning) {
                        main.postDelayed({
                            if (wantRunning && name !in queuedServiceNames) {
                                queuedServiceNames += name
                                resolveQueue.addLast(serviceInfo)
                                pumpResolveQueue()
                            }
                        }, 250L)
                    } else {
                        pumpResolveQueue()
                    }
                }
            }

            override fun onServiceResolved(serviceInfo: NsdServiceInfo) {
                main.post {
                    if (gen != resolveGen) return@post
                    main.removeCallbacks(resolveTimeout)
                    resolvingName = null
                    if (wantRunning) {
                        handleResolvedInfoLocked(serviceInfo)
                        pumpResolveQueue()
                    }
                }
            }
        }

        try {
            @Suppress("DEPRECATION")
            nsd.resolveService(next, listener)
        } catch (e: Exception) {
            Log.w(TAG, "resolveService threw for $name", e)
            main.removeCallbacks(resolveTimeout)
            resolvingName = null
            pumpResolveQueue()
        }
    }

    private fun handleResolvedInfoLocked(info: NsdServiceInfo) {
        val ctx = appContext ?: return
        val attrs = parseTxtRecords(info.attributes)
        val fp = attrs["fp"].orEmpty().lowercase().trim()
        val host = attrs["host"].orEmpty().trim()
        val tlsRaw = attrs["tls"].orEmpty().lowercase().trim()
        val tls = tlsRaw == "1" || tlsRaw == "true"
        val version = attrs["v"].orEmpty().trim()
        val port = info.port
        val addresses: List<InetAddress> = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            info.hostAddresses
        } else {
            @Suppress("DEPRECATION")
            listOfNotNull(info.host)
        }
        val resolved = ResolvedNsdService(
            serviceName = info.serviceName.orEmpty(),
            fp = fp,
            host = host,
            tls = tls,
            version = version,
            port = port,
            addresses = addresses,
        )
        if (resolved.serviceName.isNotEmpty()) {
            resolvedServices[resolved.serviceName] = resolved
        }
        applyResolvedServiceLocked(ctx, resolved)
    }

    private fun applyResolvedServiceLocked(
        ctx: Context,
        resolved: ResolvedNsdService,
        onlyIfChangedOrNew: Boolean = false,
    ) {
        val matches = matchServers(Store.servers(ctx), resolved)
        if (matches.isEmpty()) return
        for ((server, candidates) in matches) {
            val result = Store.mergeServerUrls(ctx, server.id, candidates) ?: continue
            val primaryUrl = result.preferredUrl ?: candidates.firstOrNull() ?: continue
            val discovery = DiscoveredRelay(
                serverId = result.server.id,
                url = primaryUrl,
                alts = result.server.alts,
                host = resolved.host.ifEmpty { result.server.name },
                tls = resolved.tls,
                source = "mdns",
            )
            val prev = synchronized(discoveredByServer) {
                discoveredByServer.put(result.server.id, discovery)
            }
            if (onlyIfChangedOrNew && !result.changed && prev != null && prev.url == discovery.url) {
                continue
            }
            Log.i(TAG, "matched mDNS ${resolved.serviceName} -> ${result.server.name} ($primaryUrl)")
            dispatchLocked(discovery, force = result.changed, skipClient = null)
        }
    }

    private fun dispatchLocked(discovery: DiscoveredRelay, force: Boolean, skipClient: String?) {
        val dedupeKey = "${discovery.serverId}|${discovery.url}|${discovery.source}"
        val now = SystemClock.elapsedRealtime()
        val last = lastNotifiedAt[dedupeKey]
        if (!force && last != null && now - last < NOTIFY_COOLDOWN_MS) return
        lastNotifiedAt[dedupeKey] = now
        for ((key, listener) in clients.entries.toList()) {
            if (key == skipClient) continue
            try {
                listener(discovery)
            } catch (e: Exception) {
                Log.w(TAG, "discovery listener $key failed", e)
            }
        }
    }

    // ---- Pure helpers (tested directly in JVM unit tests) ----

    internal fun parseTxtRecords(attributes: Map<String, ByteArray?>?): Map<String, String> {
        if (attributes.isNullOrEmpty()) return emptyMap()
        val out = LinkedHashMap<String, String>(attributes.size)
        for ((rawKey, rawValue) in attributes) {
            val key = rawKey.trim().lowercase()
            if (key.isEmpty()) continue
            val value = rawValue?.toString(Charsets.UTF_8)?.trim().orEmpty()
            out[key] = value
        }
        return out
    }

    internal fun selectCandidateUrls(resolved: ResolvedNsdService): List<String> {
        if (resolved.port !in 1..65535) return emptyList()
        val scheme = if (resolved.tls) "https" else "http"
        val out = LinkedHashSet<String>()

        val usableAddresses = resolved.addresses.filter { addr ->
            !addr.isAnyLocalAddress &&
                !addr.isLoopbackAddress &&
                !addr.isLinkLocalAddress &&
                !addr.isMulticastAddress
        }

        // Prefer LAN IPv4 addresses first, then other IPv4, then routable IPv6.
        val sortedAddrs = usableAddresses.sortedWith(
            compareBy<InetAddress>(
                { addr ->
                    when {
                        addr is Inet4Address && addr.isSiteLocalAddress -> 0
                        addr is Inet4Address -> 1
                        addr is Inet6Address -> 2
                        else -> 3
                    }
                },
                { it.hostAddress.orEmpty() },
            )
        )

        for (addr in sortedAddrs) {
            val hostLiteral = (addr.hostAddress ?: continue).substringBefore('%')
            val formattedHost = if (hostLiteral.contains(":")) "[$hostLiteral]" else hostLiteral
            val origin = Store.normalizeOrigin("$scheme://$formattedHost:${resolved.port}", allowLoopback = false)
            if (origin != null) out += origin
        }

        val cleanHost = sanitizeLocalHostname(resolved.host)
        if (cleanHost != null) {
            val origin = Store.normalizeOrigin("$scheme://$cleanHost:${resolved.port}", allowLoopback = false)
            if (origin != null) out += origin
        }

        return out.toList()
    }

    internal fun sanitizeLocalHostname(rawHost: String): String? {
        val cleaned = rawHost.trim().trimEnd('.').lowercase()
        if (cleaned.isEmpty() || !HOST_LABEL_REGEX.matches(cleaned)) return null
        return if (cleaned.endsWith(".local")) cleaned else "$cleaned.local"
    }

    internal fun matchServers(
        servers: List<Server>,
        resolved: ResolvedNsdService,
    ): List<Pair<Server, List<String>>> {
        val fp = resolved.fp.lowercase().trim()
        if (!FP_REGEX.matches(fp)) return emptyList()
        val candidates = selectCandidateUrls(resolved)
        if (candidates.isEmpty()) return emptyList()

        val matches = ArrayList<Pair<Server, List<String>>>()
        for (server in servers) {
            if (server.token.isEmpty()) continue
            if (Store.tokenFingerprint(server.token) != fp) continue
            // Never downgrade a pinned or HTTPS computer to HTTP.
            if (server.requiresHttps && !resolved.tls) continue
            val allowed = candidates.filter { server.allowsUrl(it) }
            if (allowed.isNotEmpty()) {
                matches += server to allowed
            }
        }
        return matches
    }
}
