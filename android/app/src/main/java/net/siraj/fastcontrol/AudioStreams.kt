package net.siraj.fastcontrol

import android.content.Context
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request
import java.io.ByteArrayInputStream
import java.io.FilterInputStream
import java.util.concurrent.TimeUnit

/** Streams authenticated relay audio without buffering the full clip in JavaScript. */
class AudioStreams(private val context: Context) {
    private val client = OkHttpClient.Builder().connectTimeout(5, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS).followRedirects(false).followSslRedirects(false).build()

    fun serve(request: WebResourceRequest): WebResourceResponse {
        if (request.method !in setOf("GET", "HEAD")) return unavailable(405)
        val uri = request.url
        val server = Store.servers(context).find { it.id == uri.getQueryParameter("server") }
            ?: return unavailable(404)
        val base = uri.getQueryParameter("base")?.trimEnd('/') ?: return unavailable(400)
        if (base !in server.urls.map { it.trimEnd('/') }) return unavailable(403)
        val path = uri.getQueryParameter("path") ?: return unavailable(400)
        return try {
            val url = base.toHttpUrl().newBuilder().addPathSegment("audio")
                .addQueryParameter("path", path).build()
            val builder = Request.Builder().url(url).method(request.method, null)
                .header("Authorization", "Bearer " + server.token)
            for (name in listOf("Range", "If-Range")) {
                request.requestHeaders.entries.find { it.key.equals(name, true) }?.let {
                    builder.header(name, it.value)
                }
            }
            val response = RelayTLS.client(server, client).newCall(builder.build()).execute()
            val body = response.body
            val mime = body?.contentType()?.toString()?.substringBefore(';') ?: ""
            if (response.code !in setOf(200, 206) || !mime.startsWith("audio/") || body == null) {
                val code = if (response.code in 400..599) response.code else 502
                response.close()
                unavailable(code)
            } else {
                val headers = mutableMapOf("Cache-Control" to "no-store", "X-Content-Type-Options" to "nosniff")
                for (name in listOf("Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified")) {
                    response.header(name)?.let { headers[name] = it }
                }
                // WebView closes the stream when playback, seeking or activity destruction ends the request.
                val start = response.header("Content-Range")?.substringAfter("bytes ")
                    ?.substringBefore('-')?.toLongOrNull() ?: 0L
                val stream = object : FilterInputStream(body.byteStream()) {
                    private var leadingSkip = start
                    // WebView applies the requested range offset to intercepted streams too.
                    // The relay already sought there, so acknowledge that initial skip once.
                    override fun skip(count: Long): Long {
                        val covered = minOf(maxOf(0L, count), leadingSkip)
                        leadingSkip -= covered
                        return covered + if (count > covered) super.skip(count - covered) else 0L
                    }
                    override fun read(): Int { leadingSkip = 0; return super.read() }
                    override fun read(bytes: ByteArray, offset: Int, length: Int): Int {
                        leadingSkip = 0; return super.read(bytes, offset, length)
                    }
                }
                WebResourceResponse(mime, null, response.code, if (response.code == 206) "Partial Content" else "OK",
                    headers, stream)
            }
        } catch (_: Exception) { unavailable(502) }
    }

    private fun unavailable(code: Int) = WebResourceResponse("text/plain", "utf-8", code,
        "Audio Unavailable", mapOf("Cache-Control" to "no-store"), ByteArrayInputStream(ByteArray(0)))
}
