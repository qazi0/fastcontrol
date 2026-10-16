package net.siraj.fastcontrol

import android.util.Base64
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.util.concurrent.TimeUnit

class ImageUploads(private val emit: (JSONObject) -> Unit) {
    private val client = OkHttpClient.Builder().connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(45, TimeUnit.SECONDS).callTimeout(55, TimeUnit.SECONDS).build()

    fun upload(id: String, server: Server?, base: String, mime: String, encoded: String) {
        Thread({
            val result = JSONObject().put("id", id)
            try {
                check(server != null && base.trimEnd('/') in server.urls.map { it.trimEnd('/') }) {
                    "Reconnect To This Computer And Try Again"
                }
                check(mime in setOf("image/jpeg", "image/png", "image/webp", "image/gif", "image/heic")) {
                    "Choose a JPEG, PNG, WebP, GIF or HEIC Image"
                }
                check(encoded.length <= 34952536) { "Image Must Be Smaller Than 25 MB" }
                val bytes = Base64.decode(encoded, Base64.NO_WRAP)
                check(bytes.isNotEmpty() && bytes.size <= 25 * 1024 * 1024) { "Image Must Be Smaller Than 25 MB" }
                val request = Request.Builder().url(base.trimEnd('/') + "/upload")
                    .header("Authorization", "Bearer " + server.token)
                    .post(bytes.toRequestBody(mime.toMediaType())).build()
                client.newCall(request).execute().use { response ->
                    check(response.isSuccessful) {
                        if (response.code == 401) "Reconnect To This Computer And Try Again"
                        else "Could Not Upload Image (HTTP ${response.code})"
                    }
                    val path = JSONObject(response.body?.string() ?: "{}").optString("path")
                    check(path.isNotEmpty()) { "The Computer Did Not Return An Image Path" }
                    result.put("ok", true).put("path", path)
                }
            } catch (e: Exception) {
                result.put("ok", false).put("message", e.message ?: "Could Not Upload Image. Try Again")
            }
            emit(result)
        }, "ImageUpload").start()
    }
}
