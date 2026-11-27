package net.siraj.fastcontrol

import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.util.Log
import org.json.JSONObject

data class NetworkEvent(
    val reason: String,
    val online: Boolean,
    val wifi: Boolean,
    val vpn: Boolean,
    val ethernet: Boolean,
    val lanChanged: Boolean,
) {
    fun toJson(): JSONObject = JSONObject()
        .put("reason", reason)
        .put("online", online)
        .put("wifi", wifi)
        .put("vpn", vpn)
        .put("ethernet", ethernet)
}

/**
 * Pure network state tracker that filters out noisy capability ticks (like RSSI or bandwidth
 * changes) while detecting transport, VPN, and link-property (IP/DNS/domain) transitions.
 */
internal class NetworkStateTracker {
    data class Snapshot(
        val wifi: Boolean = false,
        val vpn: Boolean = false,
        val ethernet: Boolean = false,
        val cellular: Boolean = false,
        val hasInternet: Boolean = false,
        val linkSignature: String = "",
    ) {
        val isLan: Boolean get() = wifi || ethernet
    }

    data class Update(
        val changed: Boolean,
        val reason: String,
        val lanChanged: Boolean,
    )

    private val networks = LinkedHashMap<String, Snapshot>()

    fun onAvailable(id: String, snapshot: Snapshot): Update {
        val prev = networks.put(id, snapshot)
        if (prev == snapshot) return Update(false, "available", false)
        val lanChanged = snapshot.isLan || (prev?.isLan == true)
        return Update(true, "available", lanChanged)
    }

    fun onCapabilitiesChanged(
        id: String,
        wifi: Boolean,
        vpn: Boolean,
        ethernet: Boolean,
        cellular: Boolean,
        hasInternet: Boolean,
    ): Update {
        val prev = networks[id]
        val next = (prev ?: Snapshot()).copy(
            wifi = wifi,
            vpn = vpn,
            ethernet = ethernet,
            cellular = cellular,
            hasInternet = hasInternet,
        )
        networks[id] = next
        if (prev != null &&
            prev.wifi == next.wifi &&
            prev.vpn == next.vpn &&
            prev.ethernet == next.ethernet &&
            prev.cellular == next.cellular &&
            prev.hasInternet == next.hasInternet
        ) {
            return Update(false, "capabilities", false)
        }
        val lanChanged = (prev?.isLan != next.isLan) || next.isLan
        return Update(true, "capabilities", lanChanged)
    }

    fun onLinkPropertiesChanged(id: String, linkSignature: String): Update {
        val prev = networks[id]
        val next = (prev ?: Snapshot()).copy(linkSignature = linkSignature)
        networks[id] = next
        if (prev != null && prev.linkSignature == linkSignature) {
            return Update(false, "link", false)
        }
        return Update(true, "link", next.isLan)
    }

    fun onLost(id: String): Update {
        val prev = networks.remove(id) ?: return Update(false, "lost", false)
        return Update(true, "lost", prev.isLan)
    }

    fun currentEvent(reason: String, lanChanged: Boolean): NetworkEvent {
        var wifi = false
        var vpn = false
        var ethernet = false
        for (snap in networks.values) {
            if (snap.wifi) wifi = true
            if (snap.vpn) vpn = true
            if (snap.ethernet) ethernet = true
        }
        return NetworkEvent(
            reason = reason,
            online = networks.isNotEmpty(),
            wifi = wifi,
            vpn = vpn,
            ethernet = ethernet,
            lanChanged = lanChanged,
        )
    }

    fun clear() {
        networks.clear()
    }
}

/**
 * Debounced ConnectivityManager.NetworkCallback observer tracking Wi-Fi, Ethernet, Cellular,
 * and VPN (including Tailscale) lifecycle transitions without connect storms.
 */
class NetworkMonitor(
    context: Context,
    private val tag: String = "NetworkMonitor",
    private val debounceMs: Long = 350L,
    private val warmupMs: Long = 500L,
    private val onChange: (NetworkEvent) -> Unit,
) {
    private val appContext = context.applicationContext
    private val main = Handler(Looper.getMainLooper())
    private val tracker = NetworkStateTracker()

    private var callback: ConnectivityManager.NetworkCallback? = null
    private var startedAtMs = 0L
    private var pendingReason: String? = null
    private var pendingLanChanged = false

    private val flush = Runnable {
        val reason = pendingReason ?: return@Runnable
        val lanChanged = pendingLanChanged
        pendingReason = null
        pendingLanChanged = false
        val event = tracker.currentEvent(reason, lanChanged)
        onChange(event)
    }

    fun start() {
        if (callback != null) return
        val cm = appContext.getSystemService(ConnectivityManager::class.java) ?: return
        tracker.clear()
        startedAtMs = SystemClock.elapsedRealtime()
        pendingReason = null
        pendingLanChanged = false

        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                val id = network.toString()
                val caps = try { cm.getNetworkCapabilities(network) } catch (_: Exception) { null }
                val lp = try { cm.getLinkProperties(network) } catch (_: Exception) { null }
                val snap = snapshotFrom(caps, lp)
                main.post {
                    if (callback !== this) return@post
                    handleUpdate(tracker.onAvailable(id, snap))
                }
            }

            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
                val id = network.toString()
                val wifi = caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)
                val vpn = caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)
                val ethernet = caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)
                val cellular = caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)
                val internet = caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                main.post {
                    if (callback !== this) return@post
                    handleUpdate(tracker.onCapabilitiesChanged(id, wifi, vpn, ethernet, cellular, internet))
                }
            }

            override fun onLinkPropertiesChanged(network: Network, linkProperties: LinkProperties) {
                val id = network.toString()
                val sig = linkSignature(linkProperties)
                main.post {
                    if (callback !== this) return@post
                    handleUpdate(tracker.onLinkPropertiesChanged(id, sig))
                }
            }

            override fun onLost(network: Network) {
                val id = network.toString()
                main.post {
                    if (callback !== this) return@post
                    handleUpdate(tracker.onLost(id))
                }
            }
        }

        val request = NetworkRequest.Builder()
            .addTransportType(NetworkCapabilities.TRANSPORT_WIFI)
            .addTransportType(NetworkCapabilities.TRANSPORT_ETHERNET)
            .addTransportType(NetworkCapabilities.TRANSPORT_CELLULAR)
            .addTransportType(NetworkCapabilities.TRANSPORT_VPN)
            // Default NetworkRequest adds NET_CAPABILITY_NOT_VPN, which hides Tailscale VPN events.
            .removeCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
            .build()

        try {
            cm.registerNetworkCallback(request, cb)
            callback = cb
        } catch (e: Exception) {
            Log.w(tag, "registerNetworkCallback failed, falling back to default callback", e)
            try {
                cm.registerDefaultNetworkCallback(cb)
                callback = cb
            } catch (fallbackErr: Exception) {
                Log.w(tag, "network callback unavailable", fallbackErr)
            }
        }
    }

    fun stop() {
        main.removeCallbacks(flush)
        pendingReason = null
        pendingLanChanged = false
        val cb = callback ?: return
        callback = null
        try {
            appContext.getSystemService(ConnectivityManager::class.java)?.unregisterNetworkCallback(cb)
        } catch (_: Exception) {
        }
        tracker.clear()
    }

    private fun handleUpdate(update: NetworkStateTracker.Update) {
        if (!update.changed) return
        // Suppress the initial burst of callbacks Android emits immediately upon registration.
        if (SystemClock.elapsedRealtime() - startedAtMs < warmupMs) return
        pendingReason = mergeReason(pendingReason, update.reason)
        pendingLanChanged = pendingLanChanged || update.lanChanged
        main.removeCallbacks(flush)
        main.postDelayed(flush, debounceMs)
    }

    companion object {
        internal fun mergeReason(existing: String?, incoming: String): String {
            if (existing == null) return incoming
            if (existing == "available" && incoming != "lost") return existing
            return incoming
        }

        private fun snapshotFrom(
            caps: NetworkCapabilities?,
            lp: LinkProperties?,
        ): NetworkStateTracker.Snapshot = NetworkStateTracker.Snapshot(
            wifi = caps?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true,
            vpn = caps?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true,
            ethernet = caps?.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) == true,
            cellular = caps?.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) == true,
            hasInternet = caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) == true,
            linkSignature = lp?.let { linkSignature(it) } ?: "",
        )

        private fun linkSignature(lp: LinkProperties): String {
            val iface = lp.interfaceName.orEmpty()
            val addrs = lp.linkAddresses.map { "${it.address.hostAddress}/${it.prefixLength}" }.sorted()
            val dns = lp.dnsServers.mapNotNull { it.hostAddress }.sorted()
            val domains = lp.domains.orEmpty()
            return "$iface|$addrs|$dns|$domains"
        }
    }
}
