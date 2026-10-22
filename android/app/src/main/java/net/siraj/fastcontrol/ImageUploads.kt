package net.siraj.fastcontrol

import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.provider.OpenableColumns
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.asRequestBody
import org.json.JSONObject
import org.json.JSONArray
import java.io.File
import java.io.IOException
import java.net.URLEncoder
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

/** Snapshot provider content before uploading, without passing file bytes through WebView. */
class ImageUploads {
    companion object {
        private val worker = Executors.newFixedThreadPool(2)
        private val main = Handler(Looper.getMainLooper())
        private val pending = LinkedHashMap<String, MutableList<String>>()
        // Observer ownership is confined to the main thread. Workers publish
        // immutable events before delivery; a new Activity can replay them.
        var observer: ((String) -> Unit)? = null
        fun events(): String = synchronized(pending) {
            JSONArray(pending.values.flatten().map { JSONObject(it) }).toString()
        }
        fun acknowledge(batch: String) { synchronized(pending) { pending.remove(batch) } }
        private fun emit(event: JSONObject) {
            val json = event.toString()
            val batch = event.getString("batch")
            synchronized(pending) {
                pending.getOrPut(batch) { ArrayList() }.add(json)
                while (pending.size > 16) pending.remove(pending.keys.first())
            }
            main.post { observer?.invoke(json) }
        }
        // Picker callbacks acquire on the main thread; both upload workers release.
        // A repeated URI keeps its grant until every consumer has a private copy.
        private val grants = HashMap<Uri, Int>()
        private fun retain(ctx: Context, uri: Uri, flags: Int): Boolean = synchronized(grants) {
            if (grants.containsKey(uri)) { grants[uri] = grants.getValue(uri) + 1; return@synchronized true }
            if (flags and Intent.FLAG_GRANT_PERSISTABLE_URI_PERMISSION == 0) return@synchronized false
            try {
                ctx.contentResolver.takePersistableUriPermission(uri, Intent.FLAG_GRANT_READ_URI_PERMISSION)
                grants[uri] = 1; true
            } catch (_: SecurityException) { false }
        }
        private fun release(ctx: Context, uri: Uri) = synchronized(grants) {
            val remaining = (grants[uri] ?: return@synchronized) - 1
            if (remaining > 0) grants[uri] = remaining
            else {
                grants.remove(uri)
                try { ctx.contentResolver.releasePersistableUriPermission(uri, Intent.FLAG_GRANT_READ_URI_PERMISSION) }
                catch (_: SecurityException) { }
            }
        }
    }
    private val client = OkHttpClient.Builder().connectTimeout(4, TimeUnit.SECONDS)
        .readTimeout(45, TimeUnit.SECONDS).callTimeout(55, TimeUnit.SECONDS)
        .followRedirects(false).followSslRedirects(false).build()

    fun selected(ctx: Context, batch: String, uris: List<Uri>, server: Server?, base: String, flags: Int = 0) {
        uris.distinct().take(12).forEachIndexed { index, uri ->
            val id = "$batch:$index"
            val retained = retain(ctx, uri, flags)
            emit(JSONObject().put("id", id).put("batch", batch).put("type", "selected"))
            worker.execute {
                var file: File? = null
                val event = JSONObject().put("id", id).put("batch", batch)
                try {
                    var name = "attachment"
                    // Metadata is optional. Some gallery/cloud providers only support
                    // opening their URI, or report a generic MIME type for a photo.
                    try { ctx.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { cursor ->
                        val column = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                        if (column >= 0 && cursor.moveToFirst()) name = cursor.getString(column) ?: name
                    } } catch (_: Exception) { }
                    var mime = try { ctx.contentResolver.getType(uri)?.takeIf { it.isNotBlank() } } catch (_: Exception) { null }
                        ?: android.webkit.MimeTypeMap.getSingleton().getMimeTypeFromExtension(name.substringAfterLast('.', "").lowercase()) ?: "application/octet-stream"
                    file = File.createTempFile("attachment-", ".upload", ctx.cacheDir)
                    (ctx.contentResolver.openInputStream(uri) ?: error("Could Not Read File. Choose It Again")).use { input ->
                        file.outputStream().use { output ->
                            val buffer = ByteArray(32768); var total = 0L
                            while (true) {
                                val count = input.read(buffer); if (count < 0) break
                                total += count; check(total <= 25L * 1024 * 1024) { "File Must Be Smaller Than 25 MB" }
                                output.write(buffer, 0, count)
                            }
                        }
                    }
                    val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
                    BitmapFactory.decodeFile(file.absolutePath, bounds)
                    if (bounds.outWidth > 0 && bounds.outHeight > 0) {
                        mime = bounds.outMimeType ?: mime
                    }
                    if (mime.startsWith("image/") && mime != "image/gif") {
                        if (bounds.outWidth > 0 && bounds.outHeight > 0 &&
                            (file.length() > 400000 || mime !in setOf("image/jpeg", "image/png", "image/webp"))) {
                            var sample = 1
                            while (maxOf(bounds.outWidth, bounds.outHeight) / sample > 2048) sample *= 2
                            val bitmap = BitmapFactory.decodeFile(file.absolutePath, BitmapFactory.Options().apply { inSampleSize = sample })
                                ?: error("Could Not Read This Image")
                            var scaled: Bitmap? = null
                            val converted = File.createTempFile("attachment-", ".jpg", ctx.cacheDir)
                            try {
                                val ratio = minOf(1.0, 2048.0 / maxOf(bitmap.width, bitmap.height))
                                scaled = Bitmap.createScaledBitmap(bitmap, maxOf(1, (bitmap.width * ratio).toInt()), maxOf(1, (bitmap.height * ratio).toInt()), true)
                                converted.outputStream().use { check(scaled.compress(Bitmap.CompressFormat.JPEG, 86, it)) { "Could Not Prepare Image" } }
                                file.delete(); file = converted; mime = "image/jpeg"
                                name = name.substringBeforeLast('.', name) + ".jpg"
                            } finally {
                                if (scaled !== bitmap) scaled?.recycle()
                                bitmap.recycle()
                                if (file !== converted) converted.delete()
                            }
                        }
                    }
                    uploadFile(file, server, base, mime, name, event)
                } catch (e: Exception) {
                    event.put("ok", false).put("message", e.message ?: "Could Not Upload File. Try Again")
                } finally {
                    file?.delete()
                    if (retained) release(ctx, uri)
                }
                emit(event)
            }
        }
        emit(JSONObject().put("batch", batch).put("type", "selectionComplete"))
    }

    private fun uploadFile(file: File, server: Server?, base: String, mime: String, name: String, result: JSONObject) {
        check(server != null && base.trimEnd('/') in server.urls.map { it.trimEnd('/') }) { "Reconnect To This Computer And Try Again" }
        val urls = (listOf(base) + server.urls).map { it.trimEnd('/') }.distinct()
        var failure: IOException? = null
        for (url in urls) {
            val request = Request.Builder().url("$url/upload").header("Authorization", "Bearer " + server.token)
                .header("X-File-Name", URLEncoder.encode(name, "UTF-8"))
                .post(file.asRequestBody(mime.toMediaType())).build()
            try {
                client.newCall(request).execute().use { response ->
                    if (response.code >= 500) throw IOException("The Computer Could Not Receive The File (HTTP ${response.code})")
                    check(response.isSuccessful) {
                        if (response.code == 401) "Reconnect To This Computer And Try Again"
                        else response.body?.string()?.take(160)?.trim().orEmpty().ifEmpty { "Could Not Upload File (HTTP ${response.code})" }
                    }
                    val data = JSONObject(response.body?.string() ?: "{}")
                    check(data.optString("path").isNotEmpty()) { "The Computer Did Not Return A File Path" }
                    result.put("ok", true).put("path", data.getString("path")).put("base", url)
                        .put("name", name).put("kind", data.optString("kind", if (mime.startsWith("image/")) "image" else "file"))
                    return
                }
            } catch (e: IOException) { failure = e }
        }
        throw IOException("Could Not Reach This Computer. Check Its Connection And Try Again", failure)
    }
}
