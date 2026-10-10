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

class ImageDownloads(private val context: Context, private val emit: (JSONObject) -> Unit,
    private val chooseFile: (File, String) -> Unit) {
    private val client = OkHttpClient.Builder().connectTimeout(5, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS).callTimeout(45, TimeUnit.SECONDS).build()

    fun download(url: String, token: String? = null) {
        Thread({
            var temporary: File? = null
            var inserted: android.net.Uri? = null
            try {
                val builder = Request.Builder().url(url)
                if (token != null) builder.header("Authorization", "Bearer $token")
                client.newCall(builder.build()).execute().use { response ->
                    check(response.isSuccessful) { "Image Is Unavailable" }
                    val body = response.body ?: error("Image Is Unavailable")
                    val mime = body.contentType()?.toString()?.substringBefore(';') ?: ""
                    val ext = mapOf("image/png" to "png", "image/jpeg" to "jpg", "image/webp" to "webp", "image/gif" to "gif")[mime]
                        ?: error("Unsupported Image Format")
                    check(body.contentLength() <= 10 * 1024 * 1024) { "Image Is Too Large" }
                    val file = File.createTempFile("image-", ".$ext", context.cacheDir); temporary = file
                    file.outputStream().use { out -> body.byteStream().use { input ->
                        val buffer = ByteArray(32768); var total = 0
                        while (true) { val n = input.read(buffer); if (n < 0) break; total += n
                            check(total <= 10 * 1024 * 1024) { "Image Is Too Large" }; out.write(buffer, 0, n) }
                    } }
                    if (Build.VERSION.SDK_INT >= 29) {
                        val resolver = context.contentResolver
                        val values = ContentValues().apply {
                            put(MediaStore.MediaColumns.DISPLAY_NAME, "Image-${System.currentTimeMillis()}.$ext")
                            put(MediaStore.MediaColumns.MIME_TYPE, mime)
                            put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/fastcontrol")
                            put(MediaStore.MediaColumns.IS_PENDING, 1)
                        }
                        val uri = resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values) ?: error("Could Not Save Image")
                        inserted = uri
                        (resolver.openOutputStream(uri) ?: error("Could Not Save Image")).use { out -> file.inputStream().use { it.copyTo(out) } }
                        resolver.update(uri, ContentValues().apply { put(MediaStore.MediaColumns.IS_PENDING, 0) }, null, null)
                        inserted = null
                        emit(JSONObject().put("ok", true).put("message", "Image Saved to Downloads"))
                    } else {
                        chooseFile(file, mime); temporary = null
                    }
                }
            } catch (_: Exception) {
                inserted?.let { context.contentResolver.delete(it, null, null) }
                emit(JSONObject().put("ok", false).put("message", "Could Not Download Image"))
            } finally { temporary?.delete() }
        }, "ImageDownload").start()
    }
}
