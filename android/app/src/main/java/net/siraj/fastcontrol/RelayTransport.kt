package net.siraj.fastcontrol

import android.content.Context
import android.os.Handler
import android.os.Looper
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.io.FilterInputStream
import java.util.concurrent.TimeUnit

/** Keeps pinned HTTPS and WebSockets on the existing native networking stack. */
class RelayTransport(private val context: Context, private val emit: (JSONObject) -> Unit) {
    private val main = Handler(Looper.getMainLooper())
    private val http = OkHttpClient.Builder().connectTimeout(5, TimeUnit.SECONDS)
        .readTimeout(60, TimeUnit.SECONDS).followRedirects(false).followSslRedirects(false).build()
    private val socketClient = http.newBuilder().readTimeout(0, TimeUnit.SECONDS).pingInterval(25, TimeUnit.SECONDS).build()
    // The bridge publishes an immutable candidate. Intercept workers only read it.
    @Volatile private var pairing: Server? = null
    // Socket entries and disposal are owned by the main thread. Network callbacks post there.
    private class Connection { var socket: WebSocket? = null }
    private val sockets = HashMap<String, Connection>()
    private var disposed = false

    fun preparePairing(url: String, token: String, pin: String): Boolean {
        val parsed = url.toHttpUrlOrNull() ?: return false
        if (parsed.scheme != "https" || parsed.encodedPath != "/" || parsed.query != null ||
            parsed.username.isNotEmpty() || parsed.password.isNotEmpty() || !RelayTLS.validPin(pin)) return false
        pairing = Server("pairing", "", url.trimEnd('/'), token, pin = pin)
        return true
    }
    fun clearPairing() { pairing = null }
    private fun origin(url: String): String? = url.toHttpUrlOrNull()?.let { "${it.scheme}://${it.host}:${it.port}" }
    private fun paired(url: String, auth: Boolean = false): Server? {
        val address = origin(url) ?: return null
        if (auth) pairing?.let { if (origin(it.url) == address) return it }
        return Store.servers(context).firstOrNull { it.pin.isNotEmpty() && it.urls.any { base -> origin(base) == address } }
    }
    fun intercept(request: WebResourceRequest): WebResourceResponse? {
        val url = request.url.toString()
        val path = request.url.path
        val server = paired(url, path == "/auth") ?: return null
        val headers = mutableMapOf("Access-Control-Allow-Origin" to "https://${MainActivity.ASSET_HOST}",
            "Access-Control-Allow-Headers" to "Authorization, Content-Type, Range, If-Range",
            "Access-Control-Allow-Methods" to "GET, HEAD, POST, OPTIONS", "Cache-Control" to "no-store")
        fun error(code: Int, message: String) = WebResourceResponse("text/plain", "utf-8", code, "Relay Error", headers, ByteArrayInputStream(message.toByteArray()))
        if (request.method == "OPTIONS") return WebResourceResponse("text/plain", "utf-8", 204, "No Content", headers, ByteArrayInputStream(byteArrayOf()))
        if (request.method !in setOf("GET", "HEAD") && !(request.method == "POST" && path == "/speech/install")) return error(405, "Use the native upload controls")
        if (path !in setOf("/auth", "/usage", "/image", "/audio", "/file", "/transcribe", "/speech/install")) return error(404, "Unknown relay endpoint")
        return try {
            val builder = Request.Builder().url(url).header("Authorization", "Bearer " + server.token)
                .method(request.method, if (request.method == "POST") byteArrayOf().toRequestBody() else null)
            for (name in listOf("Range", "If-Range")) request.requestHeaders.entries.find { it.key.equals(name, true) }?.let { builder.header(name, it.value) }
            val response = RelayTLS.client(server, http).newCall(builder.build()).execute()
            if (response.isRedirect) { response.close(); return error(502, "Relay redirects are not accepted") }
            val body = response.body
            val type = body?.contentType()
            for (name in listOf("Content-Length", "Content-Range", "Accept-Ranges", "ETag")) response.header(name)?.let { headers[name] = it }
            val stream = if (body == null) ByteArrayInputStream(byteArrayOf()) else object : FilterInputStream(body.byteStream()) {
                override fun close() { try { super.close() } finally { response.close() } }
            }
            WebResourceResponse(type?.let { "${it.type}/${it.subtype}" } ?: "application/octet-stream", "utf-8", response.code,
                response.message.ifEmpty { "Relay Response" }, headers, stream)
        } catch (_: Exception) { error(502, "Secure connection failed. Check the computer or pair it again") }
    }
    fun open(id: String, serverId: String, base: String) = main.post {
        if (disposed) return@post
        val server = Store.servers(context).firstOrNull { it.id == serverId && it.pin.isNotEmpty() && base.trimEnd('/') in it.urls.map { url -> url.trimEnd('/') } }
        if (server == null || !base.startsWith("https://")) { emit(JSONObject().put("id", id).put("event", "close")); return@post }
        sockets.remove(id)?.socket?.cancel()
        val connection = Connection(); sockets[id] = connection
        fun deliver(event: String, text: String = "", last: Boolean = false) = main.post delivery@{
            if (disposed || sockets[id] !== connection) return@delivery
            if (event == "close") sockets.remove(id)
            emit(JSONObject().put("id", id).put("event", event).put("text", text).put("last", last))
        }
        try {
            val request = Request.Builder().url(base.trimEnd('/').replaceFirst("https://", "wss://") + "/ws")
                .header("Authorization", "Bearer " + server.token).build()
            connection.socket = RelayTLS.client(server, socketClient).newWebSocket(request, object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) { deliver("open") }
                override fun onMessage(webSocket: WebSocket, text: String) {
                    if (text.length <= 65536) { deliver("message", text); return }
                    var start = 0
                    while (start < text.length) {
                        var end = minOf(start + 65536, text.length)
                        if (end < text.length && Character.isHighSurrogate(text[end - 1])) end--
                        deliver("chunk", text.substring(start, end), end == text.length)
                        start = end
                    }
                }
                override fun onClosing(webSocket: WebSocket, code: Int, reason: String) { webSocket.close(1000, null); deliver("close") }
                override fun onClosed(webSocket: WebSocket, code: Int, reason: String) { deliver("close") }
                override fun onFailure(webSocket: WebSocket, error: Throwable, response: Response?) { response?.close(); deliver("close") }
            })
        } catch (_: Exception) { deliver("close") }
    }
    fun send(id: String, text: String) = main.post { sockets[id]?.socket?.send(text) }
    fun close(id: String) = main.post { sockets.remove(id)?.socket?.cancel() }
    fun dispose() { disposed = true; pairing = null; val active = sockets.values.toList(); sockets.clear(); active.forEach { it.socket?.cancel() } }
}
