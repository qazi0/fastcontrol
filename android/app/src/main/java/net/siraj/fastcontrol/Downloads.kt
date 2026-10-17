package net.siraj.fastcontrol

import android.content.ContentValues
import android.content.Context
import android.os.Build
import android.os.Environment
import android.provider.MediaStore
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONObject
import java.io.File
import java.util.concurrent.TimeUnit

class Downloads(private val context: Context, private val emit: (JSONObject) -> Unit,
    private val chooseFile: (File, String, String) -> Unit) {
    private val client = OkHttpClient.Builder().connectTimeout(5, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS).build()
    private val pairedClient = client.newBuilder().followRedirects(false).followSslRedirects(false).build()

    fun download(url: String, token: String? = null, fileName: String? = null) {
        Thread({
            val label = if (fileName == null) "Image" else "File"
            var temporary: File? = null
            var inserted: android.net.Uri? = null
            try {
                val builder = Request.Builder().url(url)
                if (token != null) builder.header("Authorization", "Bearer $token")
                (if (token == null) client else pairedClient).newCall(builder.build()).execute().use { response ->
                    check(response.isSuccessful) { "$label Is Unavailable" }
                    val body = response.body ?: error("$label Is Unavailable")
                    val mime = body.contentType()?.toString()?.substringBefore(';') ?: "application/octet-stream"
                    val ext = mapOf("image/png" to "png", "image/jpeg" to "jpg", "image/webp" to "webp", "image/gif" to "gif")[mime]
                    if (fileName == null) {
                        check(ext != null) { "Unsupported Image Format" }
                        check(body.contentLength() <= 10 * 1024 * 1024) { "Image Is Too Large" }
                    }
                    val name = fileName?.replace('\\', '/')?.substringAfterLast('/')
                        ?.filter { it >= ' ' && it != '\u007f' }?.take(200)?.ifBlank { "Download" }
                        ?: "Image-${System.currentTimeMillis()}.$ext"
                    fun transfer(out: java.io.OutputStream) {
                        body.byteStream().use { input ->
                            val buffer = ByteArray(32768); var total = 0L
                            while (true) {
                                val n = input.read(buffer); if (n < 0) break; total += n
                                if (fileName == null) check(total <= 10L * 1024 * 1024) { "Image Is Too Large" }
                                out.write(buffer, 0, n)
                            }
                            check(body.contentLength() < 0 || total == body.contentLength()) { "Incomplete Download" }
                        }
                    }
                    if (Build.VERSION.SDK_INT >= 29) {
                        val resolver = context.contentResolver
                        val values = ContentValues().apply {
                            put(MediaStore.MediaColumns.DISPLAY_NAME, name)
                            put(MediaStore.MediaColumns.MIME_TYPE, mime)
                            put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/fastcontrol")
                            put(MediaStore.MediaColumns.IS_PENDING, 1)
                        }
                        val uri = resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values) ?: error("Could Not Save $label")
                        inserted = uri
                        (resolver.openOutputStream(uri) ?: error("Could Not Save $label")).use { transfer(it) }
                        check(resolver.update(uri, ContentValues().apply { put(MediaStore.MediaColumns.IS_PENDING, 0) }, null, null) > 0)
                        inserted = null
                        emit(JSONObject().put("ok", true).put("message", "$label Saved to Downloads"))
                    } else {
                        val file = File.createTempFile("download-", ".tmp", context.cacheDir); temporary = file
                        file.outputStream().use { transfer(it) }
                        chooseFile(file, mime, name); temporary = null
                    }
                }
            } catch (_: Exception) {
                inserted?.let { context.contentResolver.delete(it, null, null) }
                emit(JSONObject().put("ok", false).put("message", "Could Not Download $label. Check The Computer Connection And Try Again"))
            } finally { temporary?.delete() }
        }, "FileDownload").start()
    }
}
