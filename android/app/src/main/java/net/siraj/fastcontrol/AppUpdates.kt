package net.siraj.fastcontrol

import android.content.ContentProvider
import android.content.ContentValues
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInfo
import android.content.pm.PackageManager
import android.database.Cursor
import android.database.MatrixCursor
import android.net.Uri
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.provider.OpenableColumns
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONObject
import java.io.File
import java.io.FileNotFoundException
import java.io.ByteArrayOutputStream
import java.security.MessageDigest
import java.util.concurrent.TimeUnit

/** One update operation per process. Mutable state and observers belong to the main thread. */
object AppUpdates {
    private const val REPO = "https://github.com/qazi0/fastcontrol"
    private const val DAY = 24 * 60 * 60 * 1000L
    private val main = Handler(Looper.getMainLooper())
    private val client = OkHttpClient.Builder().connectTimeout(8, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS).callTimeout(3, TimeUnit.MINUTES)
        .followSslRedirects(false).build()
    private lateinit var context: Context
    private lateinit var installed: PackageInfo
    private var release: Release? = null
    private var working = false
    private var phase = "idle"
    var observer: ((String) -> Unit)? = null
    @Volatile var snapshot = "{}"
        private set

    private data class Release(val version: String, val url: String, val hash: String, val size: Long)
    private fun prefs() = context.getSharedPreferences("app-updates", Context.MODE_PRIVATE)
    private fun apk() = File(context.cacheDir, "updates/update.apk")
    private fun flags() = if (Build.VERSION.SDK_INT >= 28) PackageManager.GET_SIGNING_CERTIFICATES else PackageManager.GET_SIGNATURES
    @Suppress("DEPRECATION")
    private fun code(info: PackageInfo) = if (Build.VERSION.SDK_INT >= 28) info.longVersionCode else info.versionCode.toLong()
    @Suppress("DEPRECATION")
    private fun signers(info: PackageInfo): Set<String> = (if (Build.VERSION.SDK_INT >= 28) info.signingInfo?.apkContentsSigners else info.signatures)
        ?.map { hex(MessageDigest.getInstance("SHA-256").digest(it.toByteArray())) }?.toSet() ?: emptySet()
    private fun hex(bytes: ByteArray) = bytes.joinToString("") { "%02x".format(it.toInt() and 255) }
    private fun newer(version: String): Boolean {
        val a = version.split('.').map { it.toLong() }
        val b = (installed.versionName ?: "0.0.0").split('.').map { it.toLongOrNull() ?: 0 }
        for (i in 0..2) if (a[i] != b.getOrElse(i) { 0 }) return a[i] > b.getOrElse(i) { 0 }
        return false
    }
    private fun readRelease(data: JSONObject): Release {
        val tag = data.getString("tag_name")
        val version = tag.removePrefix("v")
        kotlin.check(Regex("[0-9]{1,8}\\.[0-9]{1,8}\\.[0-9]{1,8}").matches(version) && !data.optBoolean("draft") && !data.optBoolean("prerelease"))
        val name = "fastcontrol_android_v$version.apk"
        val assets = data.getJSONArray("assets")
        val asset = (0 until assets.length()).map { assets.getJSONObject(it) }.single { it.optString("name") == name }
        val url = asset.getString("browser_download_url")
        kotlin.check(url == "$REPO/releases/download/$tag/$name")
        val hash = asset.getString("digest").removePrefix("sha256:")
        val size = asset.getLong("size")
        kotlin.check(Regex("[a-fA-F0-9]{64}").matches(hash) && size in 1..100L * 1024 * 1024)
        return Release(version, url, hash.lowercase(), size)
    }
    fun initialize(ctx: Context) {
        if (::context.isInitialized) return
        context = ctx.applicationContext
        installed = context.packageManager.getPackageInfo(context.packageName, flags())
        File(apk().parentFile, "download.part").delete()
        val cached = prefs().getString("release", null)
        if (cached != null) try { release = readRelease(JSONObject(cached)) } catch (_: Exception) { prefs().edit().remove("release").apply() }
        phase = when {
            release?.let { newer(it.version) } != true -> "idle"
            apk().isFile && runCatching { validate(apk(), release!!) }.isSuccess -> "ready"
            else -> "available"
        }
        if (phase == "idle") apk().delete()
        publish()
    }
    private fun publish(message: String = "", progress: Int = 0, automatic: Boolean = false) {
        val candidate = release
        snapshot = JSONObject().put("phase", phase).put("installed", installed.versionName)
            .put("version", candidate?.version ?: "").put("size", candidate?.size ?: 0)
            .put("url", candidate?.url ?: "$REPO/releases/latest")
            .put("message", message).put("progress", progress).put("automatic", automatic)
            .put("offer", candidate != null && newer(candidate.version) && System.currentTimeMillis() >= prefs().getLong("later", 0))
            .toString()
        observer?.invoke(snapshot)
    }
    fun later() { prefs().edit().putLong("later", System.currentTimeMillis() + DAY).apply(); publish() }
    fun checkForUpdates(manual: Boolean) {
        if (working || phase == "installing") return
        if (!manual && System.currentTimeMillis() - prefs().getLong("checked", 0) < DAY) { publish(automatic = true); return }
        if (!manual && System.currentTimeMillis() - prefs().getLong("attempted", 0) < TimeUnit.HOURS.toMillis(1)) return
        prefs().edit().putLong("attempted", System.currentTimeMillis()).apply()
        working = true; phase = "checking"; publish(automatic = !manual)
        Thread({
            try {
                val request = Request.Builder().url("https://api.github.com/repos/qazi0/fastcontrol/releases/latest")
                    .header("Accept", "application/vnd.github+json").header("User-Agent", "fastcontrol/${installed.versionName}")
                    .header("X-GitHub-Api-Version", "2022-11-28")
                prefs().getString("etag", null)?.let { request.header("If-None-Match", it) }
                var etag: String? = null
                val data = client.newCall(request.build()).execute().use { response ->
                    if (response.code == 304) JSONObject(prefs().getString("release", null) ?: error("No Cached Release"))
                    else {
                        kotlin.check(response.isSuccessful) { "Could Not Check GitHub. Try Again Later" }
                        etag = response.header("ETag")
                        val body = response.body ?: error("Empty Release")
                        val text = body.byteStream().use { input -> ByteArrayOutputStream().use { out ->
                            val buffer = ByteArray(8192)
                            while (true) { val n = input.read(buffer); if (n < 0) break; kotlin.check(out.size() + n <= 1024 * 1024); out.write(buffer, 0, n) }
                            out.toByteArray()
                        } }
                        kotlin.check(text.size <= 1024 * 1024)
                        JSONObject(String(text, Charsets.UTF_8))
                    }
                }
                val found = readRelease(data)
                main.post {
                    release = found; working = false
                    prefs().edit().putLong("checked", System.currentTimeMillis()).putString("release", data.toString())
                        .putString("etag", etag ?: prefs().getString("etag", null)).apply()
                    phase = if (!newer(found.version)) "current" else if (apk().isFile && runCatching { validate(apk(), found) }.isSuccess) "ready" else "available"
                    publish(automatic = !manual)
                }
            } catch (_: Exception) { main.post {
                working = false
                phase = if (release?.let { newer(it.version) && apk().isFile && runCatching { validate(apk(), it) }.isSuccess } == true) "ready" else "error"
                publish("Could Not Check For Updates. Check Your Connection And Try Again", automatic = !manual)
            } }
        }, "AppUpdateCheck").start()
    }
    fun download() {
        if (working || phase == "installing") return
        val candidate = release ?: return
        if (!newer(candidate.version)) return
        working = true; phase = "downloading"; publish()
        Thread({
            val file = File(apk().parentFile, "download.part")
            try {
                file.parentFile!!.mkdirs()
                client.newCall(Request.Builder().url(candidate.url).build()).execute().use { response ->
                    val host = response.request.url.host
                    kotlin.check(response.isSuccessful && response.request.url.scheme == "https" && (host == "github.com" || host.endsWith(".githubusercontent.com")))
                    val body = response.body ?: error("Empty Download")
                    var count = 0L; var last = 0L
                    body.byteStream().use { input -> file.outputStream().use { out ->
                        val buffer = ByteArray(32768)
                        while (true) {
                            val n = input.read(buffer); if (n < 0) break
                            count += n; kotlin.check(count <= candidate.size)
                            out.write(buffer, 0, n)
                            if (System.currentTimeMillis() - last > 100) {
                                last = System.currentTimeMillis(); val percent = (count * 100 / candidate.size).toInt()
                                main.post { publish(progress = percent) }
                            }
                        }
                    } }
                    kotlin.check(count == candidate.size)
                }
                validate(file, candidate)
                kotlin.check(file.renameTo(apk()))
                main.post { working = false; phase = "ready"; publish() }
            } catch (_: Exception) {
                file.delete()
                main.post { working = false; phase = "error"; publish("Could Not Download Or Verify The Update. Try Again Or Download From GitHub") }
            }
        }, "AppUpdateDownload").start()
    }
    private fun validate(file: File, candidate: Release) {
        kotlin.check(file.length() == candidate.size)
        val digest = MessageDigest.getInstance("SHA-256")
        file.inputStream().use { input -> val buffer = ByteArray(32768); while (true) { val n = input.read(buffer); if (n < 0) break; digest.update(buffer, 0, n) } }
        kotlin.check(hex(digest.digest()) == candidate.hash)
        val info = context.packageManager.getPackageArchiveInfo(file.path, flags()) ?: error("Invalid APK")
        kotlin.check(info.packageName == context.packageName && info.versionName == candidate.version && code(info) > code(installed))
        val signatures = signers(info)
        kotlin.check(signatures.isNotEmpty() && signatures == signers(installed))
    }
    fun voiceBusy(): Boolean = runCatching { JSONObject(VoiceCaptureService.snapshot(context)).optString("phase") in setOf("preparing", "recording", "transcribing") }.getOrDefault(true)
    fun installIntent(): Intent? {
        if (working || voiceBusy()) { publish("Finish Voice Recording Or Transcription Before Installing"); return null }
        val candidate = release ?: return null
        if (!newer(candidate.version)) { phase = "current"; publish(); return null }
        if (runCatching { validate(apk(), candidate) }.isFailure) { phase = "available"; publish("Download The Update Again Before Installing"); return null }
        phase = "installing"; publish()
        return Intent(Intent.ACTION_VIEW).setDataAndType(Uri.parse("content://${context.packageName}.updates/update.apk"), "application/vnd.android.package-archive")
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
    }
    fun installReturned() { if (phase == "installing") { phase = "ready"; publish() } }
    fun installBlocked(message: String) { publish(message) }
}

/** Only the staged update can be shared, and only with an explicit temporary read grant. */
class UpdateFileProvider : ContentProvider() {
    override fun onCreate() = true
    private fun file(uri: Uri): File {
        if (uri.authority != context!!.packageName + ".updates" || uri.path != "/update.apk") throw FileNotFoundException()
        return File(context!!.cacheDir, "updates/update.apk").also { if (!it.isFile) throw FileNotFoundException() }
    }
    override fun getType(uri: Uri): String { file(uri); return "application/vnd.android.package-archive" }
    override fun openFile(uri: Uri, mode: String): ParcelFileDescriptor {
        if (mode != "r") throw FileNotFoundException()
        return ParcelFileDescriptor.open(file(uri), ParcelFileDescriptor.MODE_READ_ONLY)
    }
    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, selectionArgs: Array<out String>?, sortOrder: String?): Cursor {
        val f = file(uri); val columns = projection ?: arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE)
        return MatrixCursor(columns).apply { addRow(columns.map { when (it) { OpenableColumns.DISPLAY_NAME -> "fastcontrol_update.apk"; OpenableColumns.SIZE -> f.length(); else -> null } }) }
    }
    override fun insert(uri: Uri, values: ContentValues?): Uri? = throw UnsupportedOperationException()
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = throw UnsupportedOperationException()
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int = throw UnsupportedOperationException()
}
