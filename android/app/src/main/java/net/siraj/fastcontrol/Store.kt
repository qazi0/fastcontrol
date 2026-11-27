package net.siraj.fastcontrol

import android.content.Context
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import org.json.JSONArray
import org.json.JSONObject
import java.net.Inet4Address
import java.net.Inet6Address
import java.net.InetAddress
import java.security.MessageDigest

/** One paired relay, as handed over by the web UI through `CodexAndroid.setServers`. */
data class Server(
    val id: String,
    val name: String,
    val url: String,
    val token: String,
    val alts: List<String> = emptyList(),
    val pin: String = "",
) {
    val requiresHttps: Boolean
        get() = pin.isNotEmpty() || url.startsWith("https://", ignoreCase = true)

    val fingerprint: String
        get() = Store.tokenFingerprint(token)

    fun allowsUrl(candidate: String): Boolean {
        val normalized = Store.normalizeOrigin(candidate, allowLoopback = true) ?: return false
        return !requiresHttps || normalized.startsWith("https://")
    }

    /** The paired address first, then the other addresses the relay advertised. */
    val urls: List<String>
        get() = (listOf(url) + alts)
            .map { it.trimEnd('/') }
            .filter { it.isNotEmpty() && (!requiresHttps || it.startsWith("https://", ignoreCase = true)) }
            .distinct()
}

/** Persistent settings shared by the activity, the bridge and the watch service. */
object Store {
    private const val FILE = "relay"
    private const val KEY_SERVERS = "servers"
    private const val KEY_NOTIFY = "notifications"
    const val MAX_ALTS = 8

    private val lock = Any()

    data class MergeResult(
        val server: Server,
        val changed: Boolean,
        val preferredUrl: String?,
    )

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE)

    fun serversJson(ctx: Context): String = synchronized(lock) {
        prefs(ctx).getString(KEY_SERVERS, "[]") ?: "[]"
    }

    fun setServersJson(ctx: Context, json: String) {
        synchronized(lock) {
            prefs(ctx).edit().putString(KEY_SERVERS, json).apply()
            RelayTLS.reset()
        }
    }

    fun servers(ctx: Context): List<Server> = parseServers(serversJson(ctx))

    fun parseServers(json: String): List<Server> {
        val arr = try { JSONArray(json) } catch (_: Exception) { return emptyList() }
        val out = ArrayList<Server>()
        for (i in 0 until arr.length()) {
            val server = parseServerObject(arr.optJSONObject(i) ?: continue) ?: continue
            out += server
        }
        return out
    }

    private fun parseServerObject(o: JSONObject): Server? {
        val url = o.optString("url").trimEnd('/')
        if (url.isEmpty()) return null
        val id = o.optString("id").ifEmpty { url }
        val alts = o.optJSONArray("alts")
        val altList = if (alts == null) emptyList() else (0 until alts.length())
            .map { alts.optString(it).trimEnd('/') }
            .filter { it.isNotEmpty() }
        val pin = o.optString("pin").lowercase()
        if (pin.isNotEmpty() && (!RelayTLS.validPin(pin) || !url.startsWith("https://", ignoreCase = true))) return null
        return Server(id, o.optString("name").ifEmpty { url }, url, o.optString("token"), altList, pin)
    }

    /**
     * Non-secret 12-char lowercase hex prefix of SHA-256(trimmed UTF-8 token).
     * Used only to match mDNS TXT `fp` records; never exposes or logs raw tokens.
     */
    fun tokenFingerprint(token: String): String {
        val trimmed = token.trim()
        if (trimmed.isEmpty()) return ""
        val digest = MessageDigest.getInstance("SHA-256").digest(trimmed.toByteArray(Charsets.UTF_8))
        val sb = StringBuilder(12)
        for (i in 0 until 6) {
            val v = digest[i].toInt() and 0xff
            sb.append(HEX[v ushr 4])
            sb.append(HEX[v and 0x0f])
        }
        return sb.toString()
    }

    private const val HEX = "0123456789abcdef"

    /**
     * Normalizes a relay origin URL to match JS `new URL(u).origin` (no path/query/userinfo,
     * lowercase scheme and host, bracketed IPv6, default ports omitted).
     */
    fun normalizeOrigin(raw: String, allowLoopback: Boolean = false): String? {
        val trimmed = raw.trim()
        if (trimmed.isEmpty()) return null
        val parsed = trimmed.toHttpUrlOrNull() ?: return null
        if (parsed.scheme != "http" && parsed.scheme != "https") return null
        if (parsed.encodedPath != "/" || parsed.query != null || parsed.fragment != null) return null
        if (parsed.username.isNotEmpty() || parsed.password.isNotEmpty()) return null
        val host = parsed.host.trimEnd('.').lowercase()
        if (host.isEmpty()) return null
        if (!allowLoopback && isUnusableAdvertisedHost(host)) return null
        val formattedHost = if (host.contains(":") && !host.startsWith("[")) "[$host]" else host
        val defaultPort = HttpUrl.defaultPort(parsed.scheme)
        return if (parsed.port == defaultPort) {
            "${parsed.scheme}://$formattedHost"
        } else {
            "${parsed.scheme}://$formattedHost:${parsed.port}"
        }
    }

    private fun isUnusableAdvertisedHost(host: String): Boolean {
        if (host == "localhost" || host == "0.0.0.0" || host == "::") return true
        val stripped = host.removePrefix("[").removeSuffix("]")
        val numeric = looksLikeIpLiteral(stripped)
        if (!numeric) return false
        val addr = try { InetAddress.getByName(stripped) } catch (_: Exception) { return true }
        return addr.isAnyLocalAddress || addr.isLoopbackAddress || addr.isLinkLocalAddress || addr.isMulticastAddress
    }

    private fun looksLikeIpLiteral(host: String): Boolean {
        if (host.contains(":")) return true
        var dots = 0
        for (ch in host) {
            if (ch == '.') dots++
            else if (ch !in '0'..'9') return false
        }
        return dots == 3
    }

    /** True for stable hostnames (.local, .ts.net) and Tailscale CGNAT/ULA addresses. */
    fun isStableFallbackHost(url: String): Boolean {
        val host = url.toHttpUrlOrNull()?.host?.trimEnd('.')?.lowercase() ?: return false
        if (host.endsWith(".local") || host.endsWith(".ts.net")) return true
        val stripped = host.removePrefix("[").removeSuffix("]")
        if (!looksLikeIpLiteral(stripped)) return false
        val addr = try { InetAddress.getByName(stripped) } catch (_: Exception) { return false }
        if (addr is Inet4Address) {
            val b = addr.address
            val first = b[0].toInt() and 0xff
            val second = b[1].toInt() and 0xff
            // Tailscale CGNAT range 100.64.0.0/10
            return first == 100 && second in 64..127
        }
        if (addr is Inet6Address) {
            val b = addr.address
            // Tailscale IPv6 ULA prefix fd7a:115c:a1e0::/48
            return (b[0].toInt() and 0xff) == 0xfd &&
                (b[1].toInt() and 0xff) == 0x7a &&
                (b[2].toInt() and 0xff) == 0x11 &&
                (b[3].toInt() and 0xff) == 0x5c &&
                (b[4].toInt() and 0xff) == 0xa1 &&
                (b[5].toInt() and 0xff) == 0xe0
        }
        return false
    }

    /**
     * Merges newly discovered/advertised URLs into a saved server's `alts` without losing
     * durable fallback routes (.local, Tailscale MagicDNS/CGNAT) and without downgrading
     * HTTPS/pinned servers to HTTP.
     */
    fun mergeAlts(
        primaryUrl: String,
        existingAlts: List<String>,
        incomingUrls: List<String>,
        requiresHttps: Boolean,
        maxAlts: Int = MAX_ALTS,
    ): List<String> {
        val primary = normalizeOrigin(primaryUrl, allowLoopback = true) ?: primaryUrl.trimEnd('/')
        fun filterList(values: List<String>, allowLoopback: Boolean): List<String> {
            val seen = LinkedHashSet<String>()
            for (raw in values) {
                val normalized = normalizeOrigin(raw, allowLoopback = allowLoopback) ?: continue
                if (requiresHttps && !normalized.startsWith("https://")) continue
                if (normalized == primary) continue
                seen += normalized
            }
            return seen.toList()
        }

        val fresh = filterList(incomingUrls, allowLoopback = false)
        val freshSet = fresh.toSet()
        val keptExisting = filterList(existingAlts, allowLoopback = false).filter { it !in freshSet }
        val stableExisting = keptExisting.filter { isStableFallbackHost(it) }
        val otherExisting = keptExisting.filterNot { isStableFallbackHost(it) }

        val merged = LinkedHashSet<String>()
        if (fresh.size + stableExisting.size <= maxAlts) {
            merged.addAll(fresh)
            merged.addAll(stableExisting)
            for (u in otherExisting) {
                if (merged.size >= maxAlts) break
                merged += u
            }
        } else {
            // Preserve durable fallbacks even when an advertisement lists many interfaces.
            val reservedStable = minOf(stableExisting.size, maxOf(2, maxAlts / 2))
            val freshLimit = (maxAlts - reservedStable).coerceAtLeast(1)
            for (u in fresh.take(freshLimit)) merged += u
            for (u in stableExisting) {
                if (merged.size >= maxAlts) break
                merged += u
            }
            for (u in fresh.drop(freshLimit)) {
                if (merged.size >= maxAlts) break
                merged += u
            }
        }
        return merged.toList()
    }

    /**
     * Pure JSON helper for updating a saved server's `alts` in-place while preserving all
     * other JSON properties (id, name, custom, url, token, pin, lastSeen, platform, etc.).
     */
    fun mergeServerUrlsInJson(
        json: String,
        serverId: String,
        incomingUrls: List<String>,
    ): Pair<String, MergeResult?> {
        val arr = try { JSONArray(json) } catch (_: Exception) { return json to null }
        for (i in 0 until arr.length()) {
            val obj = arr.optJSONObject(i) ?: continue
            val server = parseServerObject(obj) ?: continue
            if (server.id != serverId) continue

            val preferred = incomingUrls.firstNotNullOfOrNull { raw ->
                val norm = normalizeOrigin(raw, allowLoopback = false) ?: return@firstNotNullOfOrNull null
                if (server.requiresHttps && !norm.startsWith("https://")) null else norm
            }
            val mergedAlts = mergeAlts(
                primaryUrl = server.url,
                existingAlts = server.alts,
                incomingUrls = incomingUrls,
                requiresHttps = server.requiresHttps,
            )
            val changed = mergedAlts != server.alts
            if (changed) {
                val nextAlts = JSONArray()
                for (u in mergedAlts) nextAlts.put(u)
                obj.put("alts", nextAlts)
            }
            val updatedServer = if (changed) server.copy(alts = mergedAlts) else server
            return (if (changed) arr.toString() else json) to MergeResult(updatedServer, changed, preferred)
        }
        return json to null
    }

    /**
     * Thread-safely merges newly discovered or relay-advertised URLs into `Store` for `serverId`.
     * Preserves all saved server fields and respects HTTPS/pin no-downgrade rules.
     */
    fun mergeServerUrls(ctx: Context, serverId: String, incomingUrls: List<String>): MergeResult? {
        if (serverId.isEmpty() || incomingUrls.isEmpty()) return null
        synchronized(lock) {
            val currentJson = prefs(ctx).getString(KEY_SERVERS, "[]") ?: "[]"
            val (nextJson, result) = mergeServerUrlsInJson(currentJson, serverId, incomingUrls)
            if (result != null && result.changed) {
                prefs(ctx).edit().putString(KEY_SERVERS, nextJson).apply()
            }
            return result
        }
    }

    fun notificationsEnabled(ctx: Context) = prefs(ctx).getBoolean(KEY_NOTIFY, false)

    fun setNotificationsEnabled(ctx: Context, on: Boolean) {
        prefs(ctx).edit().putBoolean(KEY_NOTIFY, on).apply()
    }
}

/** Live UI state the watch service consults to avoid alerting about the thread on screen. */
object UiState {
    @Volatile var resumed = false
    @Volatile var activeServer = ""
    @Volatile var activeThread = ""
    @Volatile var activeProvider = ""

    fun isShowing(serverId: String, threadId: String, provider: String) =
        resumed && activeServer == serverId && activeThread == threadId && activeProvider == provider
}
