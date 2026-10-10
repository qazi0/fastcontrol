package net.siraj.fastcontrol

import android.content.Context
import org.json.JSONArray

/** One paired relay, as handed over by the web UI through `CodexAndroid.setServers`. */
data class Server(val id: String, val name: String, val url: String, val token: String, val alts: List<String> = emptyList()) {
    /** The paired address first, then the other addresses the relay advertised. */
    val urls: List<String> get() = (listOf(url) + alts).distinct()
}

/** Persistent settings shared by the activity, the bridge and the watch service. */
object Store {
    private const val FILE = "relay"
    private const val KEY_SERVERS = "servers"
    private const val KEY_NOTIFY = "notifications"

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE)

    fun serversJson(ctx: Context): String = prefs(ctx).getString(KEY_SERVERS, "[]") ?: "[]"

    fun setServersJson(ctx: Context, json: String) {
        prefs(ctx).edit().putString(KEY_SERVERS, json).apply()
    }

    fun servers(ctx: Context): List<Server> = parseServers(serversJson(ctx))

    fun parseServers(json: String): List<Server> {
        val arr = try { JSONArray(json) } catch (e: Exception) { return emptyList() }
        val out = ArrayList<Server>()
        for (i in 0 until arr.length()) {
            val o = arr.optJSONObject(i) ?: continue
            val url = o.optString("url")
            if (url.isEmpty()) continue
            val id = o.optString("id").ifEmpty { url }
            val alts = o.optJSONArray("alts")
            val altList = if (alts == null) emptyList() else (0 until alts.length()).map { alts.optString(it) }.filter { it.isNotEmpty() }
            out += Server(id, o.optString("name").ifEmpty { url }, url, o.optString("token"), altList)
        }
        return out
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
