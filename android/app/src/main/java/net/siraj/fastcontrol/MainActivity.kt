package net.siraj.fastcontrol

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.pm.ApplicationInfo
import android.content.pm.PackageManager
import android.content.res.Configuration
import android.graphics.Color
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Message
import android.os.VibrationEffect
import android.os.Vibrator
import android.os.VibratorManager
import android.provider.MediaStore
import android.provider.Settings
import android.view.View
import android.view.ViewGroup
import android.view.WindowInsets
import android.view.WindowInsetsController
import android.view.WindowManager
import android.webkit.JavascriptInterface
import android.webkit.RenderProcessGoneDetail
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.FrameLayout
import android.window.OnBackInvokedCallback
import android.window.OnBackInvokedDispatcher
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.io.FileNotFoundException
import java.io.File

class MainActivity : Activity() {

    companion object {
        const val ASSET_HOST = "appassets.androidplatform.net"
        const val START_URL = "https://$ASSET_HOST/index.html"
        const val ACTION_OPEN_THREAD = "net.siraj.fastcontrol.OPEN_THREAD"
        const val EXTRA_SERVER = "server"
        const val EXTRA_THREAD = "thread"
 const val EXTRA_PROVIDER = "provider"
        private const val REQ_FILES = 1
        private const val REQ_NOTIFY = 2
        private const val REQ_MIC = 3
        private const val REQ_SAVE_IMAGE = 4
        private const val REQ_ATTACHMENTS = 5
    }

    private lateinit var container: FrameLayout
    private lateinit var webView: WebView
    private val relayTransport by lazy { RelayTransport(this) { event ->
        if (!isDestroyed) webView.evaluateJavascript("window.onRelaySocket && window.onRelaySocket($event)", null)
    } }
    private var fileCallback: ValueCallback<Array<Uri>>? = null
    private var awaitingMic = false
    @Volatile private var pendingVoice: String? = null
    @Volatile private var launchingVoice: String? = null
    private var voiceCompressed = false
    private val voiceObserver: (String) -> Unit = { json ->
        if (!isDestroyed && UiState.resumed) {
            val type = JSONObject(json).optString("type")
            if (type != "level") {
                launchingVoice = null
                updateVoiceScreenState()
            }
            if (type in setOf("captureState", "transcript", "error", "cancelled")) restoreVoiceSession()
            else webView.evaluateJavascript("window.onVoiceEvent && window.onVoiceEvent($json)", null)
        }
    }
    private lateinit var downloads: Downloads
    private lateinit var uploads: ImageUploads
    private lateinit var audioStreams: AudioStreams
    private var pendingImage: File? = null
    private var pendingAttachments: String? = null
    private var pendingAppInstall = false
    private val updateObserver: (String) -> Unit = { state ->
        if (!isDestroyed) webView.evaluateJavascript("window.onAppUpdate && window.onAppUpdate($state)", null)
    }

    @Volatile private var pendingLaunch = ""

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        if ((applicationInfo.flags and ApplicationInfo.FLAG_DEBUGGABLE) != 0) {
            WebView.setWebContentsDebuggingEnabled(true)
        }

        pendingAttachments = savedInstanceState?.getString("pendingAttachments")
        pendingAppInstall = savedInstanceState?.getBoolean("pendingAppInstall") ?: false
        pendingVoice = savedInstanceState?.getString("pendingVoice")
        awaitingMic = savedInstanceState?.getBoolean("awaitingMic") ?: false
        voiceCompressed = savedInstanceState?.getBoolean("voiceCompressed") ?: false
        val bg = getColor(R.color.bg)
        container = FrameLayout(this).apply { setBackgroundColor(bg) }
        webView = WebView(this).apply {
            setBackgroundColor(bg)
            layoutParams = FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT
            )
        }
        container.addView(webView)
        setContentView(container)
        setupEdgeToEdge()
        registerBackCallback()

        webView.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            // Follow the system font size setting, which WebView ignores by default.
            textZoom = (resources.configuration.fontScale * 100).toInt()
            mixedContentMode = WebSettings.MIXED_CONTENT_ALWAYS_ALLOW
            allowFileAccess = false
            allowContentAccess = true
            setSupportMultipleWindows(true)
            javaScriptCanOpenWindowsAutomatically = false
        }
        audioStreams = AudioStreams(this)
        webView.webViewClient = ShellClient()
        webView.webChromeClient = ShellChrome()
        webView.addJavascriptInterface(Bridge(), "CodexAndroid")
        AppUpdates.initialize(this)
        uploads = ImageUploads()
        downloads = Downloads(this, { event -> runOnUiThread {
            if (!isDestroyed) webView.evaluateJavascript("window.onImageDownload && window.onImageDownload($event)", null)
        } }, { file, mime, name -> runOnUiThread {
            if (isDestroyed) { file.delete(); return@runOnUiThread }
            if (pendingImage != null) {
                file.delete()
                webView.evaluateJavascript("window.onImageDownload && window.onImageDownload({ok:false,message:'Finish Saving The Previous File First'})", null)
                return@runOnUiThread
            }
            pendingImage = file
            val save = Intent(Intent.ACTION_CREATE_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE)
                .setType(mime).putExtra(Intent.EXTRA_TITLE, name)
            try { startActivityForResult(save, REQ_SAVE_IMAGE) }
            catch (_: ActivityNotFoundException) {
                pendingImage = null; file.delete()
                webView.evaluateJavascript("window.onImageDownload && window.onImageDownload({ok:false,message:'No File Picker Is Available'})", null)
            }
        } })

        if (savedInstanceState == null) {
            pendingLaunch = describeIntent(intent)
            webView.loadUrl(START_URL)
        } else {
            pendingLaunch = savedInstanceState.getString("pendingLaunch", "")
            if (webView.restoreState(savedInstanceState) == null) webView.loadUrl(START_URL)
        }
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        webView.saveState(outState)
        outState.putString("pendingLaunch", pendingLaunch)
        outState.putString("pendingAttachments", pendingAttachments)
        outState.putBoolean("pendingAppInstall", pendingAppInstall)
        outState.putString("pendingVoice", pendingVoice)
        outState.putBoolean("awaitingMic", awaitingMic)
        outState.putBoolean("voiceCompressed", voiceCompressed)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        val launch = describeIntent(intent)
        if (launch.isNotEmpty()) {
            pendingLaunch = launch
            webView.evaluateJavascript("window.onAndroidIntent && window.onAndroidIntent()", null)
        }
    }

    override fun onResume() {
        super.onResume()
        UiState.resumed = true
        AppUpdates.observer = updateObserver
        // A force stop or app update kills the service; bring it back if the user wants alerts.
        if (Store.notificationsEnabled(this) && !RelayWatchService.isRunning) {
            startForegroundService(Intent(this, RelayWatchService::class.java))
        }
        webView.onResume()
        VoiceCaptureService.observer = voiceObserver
        ImageUploads.observer = uploadObserver
        webView.evaluateJavascript("window.restoreAttachmentUploads && window.restoreAttachmentUploads()", null)
        launchPendingVoice()
        updateVoiceScreenState()
        restoreVoiceSession()
        webView.evaluateJavascript("window.restoreAppUpdates && window.restoreAppUpdates()", null)
        AppUpdates.installReturned()
        if (pendingAppInstall) {
            pendingAppInstall = false
            if (packageManager.canRequestPackageInstalls()) requestAppInstall()
            else AppUpdates.installBlocked("Allow Updates From fastcontrol In Android Settings, Then Tap Install")
        }
    }

    override fun onPause() {
        UiState.resumed = false
        if (AppUpdates.observer === updateObserver) AppUpdates.observer = null
        webView.evaluateJavascript("window.pauseChatAudio && window.pauseChatAudio()", null)
        if (VoiceCaptureService.observer === voiceObserver) VoiceCaptureService.observer = null
        if (ImageUploads.observer === uploadObserver) ImageUploads.observer = null
        updateVoiceScreenState()
        webView.onPause()
        super.onPause()
    }

    override fun onDestroy() {
        if (AppUpdates.observer === updateObserver) AppUpdates.observer = null
        if (VoiceCaptureService.observer === voiceObserver) VoiceCaptureService.observer = null
        if (ImageUploads.observer === uploadObserver) ImageUploads.observer = null
        pendingImage?.delete(); pendingImage = null
        relayTransport.dispose()
        webView.destroy()
        super.onDestroy()
    }

    private fun updateVoiceScreenState() {
        if (isDestroyed) return
        // Read the current session on the UI thread so an old worker cannot clear a new recording's flag.
        val recording = UiState.resumed && VoiceCaptureService.isRecording
        if (recording) window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        else window.clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
    }

    private fun restoreVoiceSession() {
        if (!isDestroyed && UiState.resumed) webView.evaluateJavascript("window.restoreVoiceSession && window.restoreVoiceSession()", null)
    }

    private val uploadObserver: (String) -> Unit = { event ->
        if (!isDestroyed) webView.evaluateJavascript("window.onImageUpload && window.onImageUpload($event)", null)
    }

    private fun voiceSnapshot(): String {
        val snapshot = VoiceCaptureService.snapshot(this)
        return if (snapshot != "{}") snapshot else pendingVoice ?: launchingVoice ?: "{}"
    }

    private fun launchPendingVoice() {
        if (!UiState.resumed || awaitingMic || checkSelfPermission(Manifest.permission.RECORD_AUDIO) != PackageManager.PERMISSION_GRANTED) return
        val json = pendingVoice ?: return
        pendingVoice = null; launchingVoice = json
        try {
            startForegroundService(Intent(this, VoiceCaptureService::class.java).setAction(VoiceCaptureService.START)
                .putExtra("session", json).putExtra("compressed", voiceCompressed))
        } catch (_: Exception) {
            launchingVoice = null
            VoiceCaptureService.failStart(this, JSONObject(json), "Return To fastcontrol To Start Recording")
        }
    }

    /** Turns a launch intent into the JSON string handed to the page by consumeLaunchIntent. */
    private fun describeIntent(intent: Intent?): String {
        intent ?: return ""
        if (intent.action == ACTION_OPEN_THREAD) {
            val server = intent.getStringExtra(EXTRA_SERVER) ?: return ""
            val thread = intent.getStringExtra(EXTRA_THREAD) ?: return ""
            return JSONObject()
                .put("type", "thread").put("server", server).put("thread", thread)
 .put("provider", intent.getStringExtra(EXTRA_PROVIDER) ?: "codex")
                .toString()
        }
        val data = intent.data
        if (intent.action == Intent.ACTION_VIEW && data != null &&
            data.scheme == "fastcontrol" && data.host == "pair"
        ) {
            val url = data.getQueryParameter("url") ?: return ""
            return JSONObject()
                .put("type", "pair")
                .put("url", url)
                .put("token", data.getQueryParameter("t") ?: "")
                .put("pin", data.getQueryParameter("pin") ?: "")
                .put("name", data.getQueryParameter("name") ?: "")
                .toString()
        }
        return ""
    }

    // Insets and system bars

    private fun isNight() =
        (resources.configuration.uiMode and Configuration.UI_MODE_NIGHT_MASK) ==
            Configuration.UI_MODE_NIGHT_YES

    @Suppress("DEPRECATION")
    private fun setupEdgeToEdge() {
        val light = !isNight()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            window.setDecorFitsSystemWindows(false)
            val mask = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS or
                WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS
            window.insetsController?.setSystemBarsAppearance(if (light) mask else 0, mask)
        } else {
            var flags = View.SYSTEM_UI_FLAG_LAYOUT_STABLE or
                View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN or
                View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION
            if (light) flags = flags or View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR or
                View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR
            window.decorView.systemUiVisibility = flags
        }
        if (Build.VERSION.SDK_INT < 35) {
            window.statusBarColor = Color.TRANSPARENT
            window.navigationBarColor = Color.TRANSPARENT
        }
        container.setOnApplyWindowInsetsListener { v, insets ->
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                val i = insets.getInsets(
                    WindowInsets.Type.systemBars() or WindowInsets.Type.ime() or
                        WindowInsets.Type.displayCutout()
                )
                v.setPadding(i.left, i.top, i.right, i.bottom)
            } else {
                // With adjustResize on API 26..29 the IME height is part of the system window insets.
                v.setPadding(
                    insets.systemWindowInsetLeft, insets.systemWindowInsetTop,
                    insets.systemWindowInsetRight, insets.systemWindowInsetBottom
                )
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) WindowInsets.CONSUMED
            else insets.consumeSystemWindowInsets()
        }
        container.requestApplyInsets()
    }

    // Back navigation follows the web history (thread view and sheets use pushState).

    override fun onKeyDown(keyCode: Int, event: android.view.KeyEvent?): Boolean {
        if (keyCode == android.view.KeyEvent.KEYCODE_BACK && Build.VERSION.SDK_INT < 33 &&
            webView.canGoBack()
        ) {
            webView.goBack()
            return true
        }
        return super.onKeyDown(keyCode, event)
    }

    /** On API 33+ the system no longer sends KEYCODE_BACK, so a callback is registered for the
     * activity's lifetime. pushState does not reliably report history changes to the client,
     * so the decision is made at press time. */
    private fun registerBackCallback() {
        if (Build.VERSION.SDK_INT < 33) return
        val cb = OnBackInvokedCallback {
            if (webView.canGoBack()) {
                webView.goBack()
            } else {
                // Same as the platform default for a launcher root: keep the page alive.
                moveTaskToBack(true)
            }
        }
        onBackInvokedDispatcher.registerOnBackInvokedCallback(
            OnBackInvokedDispatcher.PRIORITY_DEFAULT, cb
        )
    }

    // External links

    private fun requestAppInstall() {
        if (!UiState.resumed || AppUpdates.voiceBusy()) {
            AppUpdates.installBlocked("Finish Voice Recording Or Transcription Before Installing")
            return
        }
        val install = AppUpdates.installIntent() ?: return
        if (!packageManager.canRequestPackageInstalls()) {
            pendingAppInstall = true
            AppUpdates.installBlocked("Allow Updates From fastcontrol In Android Settings")
            try { startActivity(Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:$packageName"))) }
            catch (_: ActivityNotFoundException) { pendingAppInstall = false; AppUpdates.installReturned(); AppUpdates.installBlocked("Open Android Settings To Allow Updates From fastcontrol") }
            return
        }
        try { startActivity(install) }
        catch (_: ActivityNotFoundException) { AppUpdates.installReturned(); AppUpdates.installBlocked("Installer Unavailable. Download The APK From GitHub") }
    }

    private fun openExternal(uri: Uri) {
        try {
            startActivity(Intent(Intent.ACTION_VIEW, uri).addCategory(Intent.CATEGORY_BROWSABLE))
        } catch (_: ActivityNotFoundException) {
        }
    }

    private inner class ShellClient : WebViewClient() {
        override fun onPageFinished(view: WebView, url: String) {
            if (!isDestroyed && UiState.resumed) view.evaluateJavascript("window.finishVoiceRestore && window.finishVoiceRestore(); window.restoreAttachmentUploads && window.restoreAttachmentUploads(); window.restoreAppUpdates && window.restoreAppUpdates()", null)
        }
        override fun shouldInterceptRequest(
            view: WebView, request: WebResourceRequest
        ): WebResourceResponse? {
            val url = request.url
            if (url.host != ASSET_HOST) return relayTransport.intercept(request)
            if (url.scheme == "https" && url.path == "/relay-audio") return audioStreams.serve(request)
            return serveAsset(url.path ?: "/")
        }

        // Without this the default is to kill the whole app when the renderer is reclaimed
        // under memory pressure. Rebuild the activity instead (localStorage survives).
        override fun onRenderProcessGone(view: WebView, detail: RenderProcessGoneDetail): Boolean {
            android.util.Log.w("fastcontrol", "renderer gone, crashed=${detail.didCrash()}")
            container.removeView(view)
            view.destroy()
            recreate()
            return true
        }

        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
            val url = request.url
            if (url.host == ASSET_HOST) return false
            openExternal(url)
            return true
        }

    }

    private fun serveAsset(rawPath: String): WebResourceResponse {
        var path = rawPath.trimStart('/')
        if (path.isEmpty() || path.endsWith("/")) path += "index.html"
        val mime = mimeFor(path)
        val encoding = if (mime.startsWith("text/") || mime.endsWith("json") ||
            mime.endsWith("javascript") || mime.endsWith("+xml")
        ) "utf-8" else null
        val headers = mapOf("Cache-Control" to "no-cache")
        return try {
            if (path.contains("..")) throw FileNotFoundException(path)
            val stream = assets.open("web/$path")
            WebResourceResponse(mime, encoding, 200, "OK", headers, stream)
        } catch (_: Exception) {
            WebResourceResponse(
                "text/plain", "utf-8", 404, "Not Found", headers,
                ByteArrayInputStream(ByteArray(0))
            )
        }
    }

    private fun mimeFor(path: String): String = when (path.substringAfterLast('.', "").lowercase()) {
        "html", "htm" -> "text/html"
        "js", "mjs" -> "text/javascript"
        "css" -> "text/css"
        "json" -> "application/json"
        "webmanifest" -> "application/manifest+json"
        "svg" -> "image/svg+xml"
        "png" -> "image/png"
        "jpg", "jpeg" -> "image/jpeg"
        "webp" -> "image/webp"
        "gif" -> "image/gif"
        "ico" -> "image/x-icon"
        "woff2" -> "font/woff2"
        "woff" -> "font/woff"
        "txt" -> "text/plain"
        else -> "application/octet-stream"
    }

    private inner class ShellChrome : WebChromeClient() {
        override fun onShowFileChooser(
            view: WebView, callback: ValueCallback<Array<Uri>>, params: FileChooserParams
        ): Boolean {
            fileCallback?.onReceiveValue(null)
            fileCallback = callback
            val type = params.acceptTypes?.firstOrNull { it.isNotBlank() } ?: "image/*"
            val pick = Intent(Intent.ACTION_GET_CONTENT).apply {
                addCategory(Intent.CATEGORY_OPENABLE)
                setType(type)
                putExtra(
                    Intent.EXTRA_ALLOW_MULTIPLE,
                    params.mode == FileChooserParams.MODE_OPEN_MULTIPLE
                )
            }
            return try {
                startActivityForResult(Intent.createChooser(pick, null), REQ_FILES)
                true
            } catch (_: ActivityNotFoundException) {
                fileCallback = null
                false
            }
        }

        // target=_blank and window.open land here; route the URL to the system browser.
        override fun onCreateWindow(
            view: WebView, isDialog: Boolean, isUserGesture: Boolean, resultMsg: Message
        ): Boolean {
            val href = view.hitTestResult.extra
            if (href != null && (href.startsWith("http://") || href.startsWith("https://"))) {
                openExternal(Uri.parse(href))
                return false
            }
            val catcher = WebView(this@MainActivity)
            catcher.webViewClient = object : WebViewClient() {
                override fun shouldOverrideUrlLoading(v: WebView, r: WebResourceRequest): Boolean {
                    openExternal(r.url)
                    v.destroy()
                    return true
                }
            }
            (resultMsg.obj as WebView.WebViewTransport).webView = catcher
            resultMsg.sendToTarget()
            return true
        }
    }

    @Deprecated("Deprecated in Java")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        if (requestCode == REQ_ATTACHMENTS) {
            val saved = pendingAttachments; pendingAttachments = null
            if (saved != null) {
                val request = JSONObject(saved)
                val uris = ArrayList<Uri>()
                if (resultCode == RESULT_OK && data != null) {
                    data.clipData?.let { clip -> for (i in 0 until minOf(clip.itemCount, 12)) uris += clip.getItemAt(i).uri }
                    if (uris.isEmpty()) data.data?.let { uris += it }
                }
                val server = Store.servers(this).find { it.id == request.optString("server") }
                uploads.selected(applicationContext, request.getString("id"), uris, server, request.getString("base"), data?.flags ?: 0)
            }
            return
        }
        if (requestCode == REQ_SAVE_IMAGE) {
            val file = pendingImage; pendingImage = null
            if (file != null) {
                Thread({
                    try {
                        if (resultCode == RESULT_OK && data?.data != null) {
                            (contentResolver.openOutputStream(data.data!!) ?: error("Could Not Save File")).use { out -> file.inputStream().use { it.copyTo(out) } }
                            runOnUiThread { if (!isDestroyed) webView.evaluateJavascript("window.onImageDownload && window.onImageDownload({ok:true,message:'File Saved'})", null) }
                        }
                    } catch (_: Exception) {
                        runOnUiThread { if (!isDestroyed) webView.evaluateJavascript("window.onImageDownload && window.onImageDownload({ok:false,message:'Could Not Save File'})", null) }
                    } finally { file.delete() }
                }, "SaveImage").start()
            }
            return
        }
        if (requestCode != REQ_FILES) {
            super.onActivityResult(requestCode, resultCode, data)
            return
        }
        val cb = fileCallback ?: return
        fileCallback = null
        if (resultCode != RESULT_OK || data == null) {
            cb.onReceiveValue(null)
            return
        }
        val uris = ArrayList<Uri>()
        data.clipData?.let { clip -> for (i in 0 until clip.itemCount) uris += clip.getItemAt(i).uri }
        if (uris.isEmpty()) data.data?.let { uris += it }
        cb.onReceiveValue(if (uris.isEmpty()) null else uris.toTypedArray())
    }

    // Watch service control

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode == REQ_MIC) {
            val wanted = awaitingMic; awaitingMic = false
            if (!wanted) return
            if (checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) launchPendingVoice()
            else {
                pendingVoice?.let { VoiceCaptureService.failStart(this, JSONObject(it), "Microphone Permission Is Required") }
                pendingVoice = null
            }
        }
    }

    private fun applyNotifications(on: Boolean) {
        if (on) {
            if (Build.VERSION.SDK_INT >= 33 &&
                checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) !=
                PackageManager.PERMISSION_GRANTED
            ) {
                requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), REQ_NOTIFY)
            }
            startForegroundService(Intent(this, RelayWatchService::class.java))
        } else {
            stopService(Intent(this, RelayWatchService::class.java))
        }
    }

    /** Exposed to the page as window.CodexAndroid. Calls arrive on a WebView binder thread. */
    inner class Bridge {
        @JavascriptInterface fun audioStreamVersion(): Int = 1
        private val ctx: Context get() = this@MainActivity

        @JavascriptInterface
        fun downloadFile(serverId: String, base: String, path: String) {
            val server = Store.servers(ctx).firstOrNull { it.id == serverId && base.trimEnd('/') in it.urls.map { u -> u.trimEnd('/') } } ?: return
            val url = base.trimEnd('/') + "/file?path=" + java.net.URLEncoder.encode(path, "UTF-8")
            downloads.download(url, server.token, path.replace('\\', '/').substringAfterLast('/'), server)
        }

        @JavascriptInterface
        fun downloadRemoteFile(url: String, name: String) {
            if (Uri.parse(url).scheme in setOf("https", "http")) downloads.download(url, fileName = name)
        }

        @JavascriptInterface
        fun prepareRelayPairing(url: String, token: String, pin: String): Boolean = relayTransport.preparePairing(url, token, pin)
        @JavascriptInterface
        fun clearRelayPairing() = relayTransport.clearPairing()
        @JavascriptInterface
        fun openRelaySocket(id: String, server: String, base: String) { relayTransport.open(id, server, base) }
        @JavascriptInterface
        fun sendRelaySocket(id: String, text: String) { relayTransport.send(id, text) }
        @JavascriptInterface
        fun closeRelaySocket(id: String) { relayTransport.close(id) }

        @JavascriptInterface
        fun getServers(): String = Store.serversJson(ctx)

        @JavascriptInterface
        fun downloadImage(serverId: String, base: String, path: String) {
            val server = Store.servers(ctx).firstOrNull { it.id == serverId && base.trimEnd('/') in it.urls.map { u -> u.trimEnd('/') } } ?: return
            val url = base.trimEnd('/') + "/image?path=" + java.net.URLEncoder.encode(path, "UTF-8")
            downloads.download(url, server.token, server = server)
        }

        @JavascriptInterface
        fun downloadRemoteImage(url: String) { if (Uri.parse(url).scheme == "https") downloads.download(url) }

        @JavascriptInterface
        fun getAttachmentEvents(): String = ImageUploads.events()

        @JavascriptInterface
        fun acknowledgeAttachments(batch: String) = ImageUploads.acknowledge(batch)

        @JavascriptInterface
        fun pickAttachments(requestId: String, serverId: String, base: String) = openAttachments(requestId, serverId, base, false)

        @JavascriptInterface
        fun pickPhotos(requestId: String, serverId: String, base: String) = openAttachments(requestId, serverId, base, true)

        private fun openAttachments(requestId: String, serverId: String, base: String, photos: Boolean) = runOnUiThread {
            if (pendingAttachments != null) return@runOnUiThread
            pendingAttachments = JSONObject().put("id", requestId).put("server", serverId).put("base", base).toString()
            try {
                val picker = if (photos && Build.VERSION.SDK_INT >= 33) Intent(MediaStore.ACTION_PICK_IMAGES).apply {
                    type = "image/*"
                    putExtra(MediaStore.EXTRA_PICK_IMAGES_MAX, 12)
                    addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
                } else Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
                    addCategory(Intent.CATEGORY_OPENABLE); type = if (photos) "image/*" else "*/*"
                    putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true)
                    addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_PERSISTABLE_URI_PERMISSION)
                }
                startActivityForResult(picker, REQ_ATTACHMENTS)
            } catch (_: ActivityNotFoundException) {
                pendingAttachments = null
                webView.evaluateJavascript("window.onImageUpload && window.onImageUpload(${JSONObject().put("batch", requestId).put("type", "selectionComplete")})", null)
            }
        }

        @JavascriptInterface
        fun getVoiceSession(): String = voiceSnapshot()

        @JavascriptInterface
        fun acknowledgeVoice(id: String) = runOnUiThread { VoiceCaptureService.acknowledge(ctx, id) }

        @JavascriptInterface
        fun updateVoiceDraft(id: String, text: String) = runOnUiThread { VoiceCaptureService.updateDraft(id, text) }

        @JavascriptInterface
        fun retryVoiceSession(id: String, serverId: String, base: String) = runOnUiThread {
            val state=try{JSONObject(voiceSnapshot())}catch (_: Exception){return@runOnUiThread}
            val server=Store.servers(ctx).find{it.id==serverId} ?: return@runOnUiThread
            if(!UiState.resumed || state.optString("id")!=id || state.optString("server")!=serverId || state.optString("phase")!="error" || !state.optBoolean("retryable") || base.trimEnd('/') !in server.urls.map{it.trimEnd('/')})return@runOnUiThread
            state.put("base",base.trimEnd('/'))
            try {startForegroundService(Intent(ctx,VoiceCaptureService::class.java).setAction(VoiceCaptureService.RETRY).putExtra("session",state.toString()))}
            catch (_: Exception) {VoiceCaptureService.failStart(ctx,state,"Could Not Retry. Your Recording Is Saved")}
        }

        @JavascriptInterface
        fun startVoice(requestId: String) = startVoiceRequest(requestId, UiState.activeServer, "", UiState.activeThread, UiState.activeProvider, false, "")

        @JavascriptInterface
        fun startVoiceCompressed(requestId: String) = startVoiceRequest(requestId, UiState.activeServer, "", UiState.activeThread, UiState.activeProvider, true, "")

        @JavascriptInterface
        fun startVoiceSession(id: String, server: String, base: String, thread: String, provider: String, compressed: Boolean, draft: String) =
            startVoiceRequest(id, server, base, thread, provider, compressed, draft)

        private fun startVoiceRequest(id: String, serverId: String, base: String, thread: String, provider: String, compressed: Boolean, draft: String) = runOnUiThread {
            val existing = try { JSONObject(voiceSnapshot()) } catch (_: Exception) { JSONObject() }
            if (existing.optString("id").isNotEmpty()) {
                if (Store.servers(ctx).none { it.id == existing.optString("server") }) {
                    VoiceCaptureService.cancel(existing.getString("id"))
                    VoiceCaptureService.acknowledge(ctx, existing.getString("id"))
                } else if (existing.optString("phase") == "cancelled") VoiceCaptureService.acknowledge(ctx, existing.getString("id"))
                else { restoreVoiceSession(); return@runOnUiThread }
            }
            val server = Store.servers(ctx).find { it.id == serverId }
            val address = base.ifEmpty { server?.url ?: "" }.trimEnd('/')
            val state = JSONObject().put("id", id).put("server", serverId).put("base", address)
                .put("thread", thread).put("provider", if (provider == "claude") "claude" else "codex")
                .put("phase", "preparing").put("draft", draft.take(65536))
            if (server == null || address !in server.urls.map { it.trimEnd('/') }) {
                VoiceCaptureService.failStart(ctx, state, "Reconnect To This Computer And Try Again"); return@runOnUiThread
            }
            if (!UiState.resumed) {
                VoiceCaptureService.failStart(ctx, state, "Return To fastcontrol To Start Recording"); return@runOnUiThread
            }
            pendingVoice = state.toString(); voiceCompressed = compressed
            if (checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) launchPendingVoice()
            else {
                awaitingMic = true
                val permissions = mutableListOf(Manifest.permission.RECORD_AUDIO)
                if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) permissions.add(Manifest.permission.POST_NOTIFICATIONS)
                requestPermissions(permissions.toTypedArray(), REQ_MIC)
            }
        }

        @JavascriptInterface
        fun stopVoice(serverId: String, base: String) = stopVoiceSession(JSONObject(voiceSnapshot()).optString("id"), serverId, base)

        @JavascriptInterface
        fun stopVoiceSession(id: String, serverId: String, base: String) {
            val server = Store.servers(ctx).find { it.id == serverId && base.trimEnd('/') in it.urls.map { u -> u.trimEnd('/') } } ?: return
            runOnUiThread { VoiceCaptureService.finish(id, server, base) }
        }

        @JavascriptInterface
        fun cancelVoice() = cancelVoiceSession(JSONObject(voiceSnapshot()).optString("id"))

        @JavascriptInterface
        fun cancelVoiceSession(id: String) = runOnUiThread {
            if (JSONObject(pendingVoice ?: "{}").optString("id") == id) { awaitingMic = false; pendingVoice = null }
            if (JSONObject(launchingVoice ?: "{}").optString("id") == id) launchingVoice = null
            VoiceCaptureService.discardSaved(ctx,id)
            VoiceCaptureService.cancel(id)
        }

        @JavascriptInterface
        fun setServers(json: String) {
            Store.setServersJson(ctx, json)
            runOnUiThread { RelayWatchService.reloadIfRunning() }
        }

        @JavascriptInterface
        fun setNotifications(on: Boolean) {
            Store.setNotificationsEnabled(ctx, on)
            runOnUiThread { applyNotifications(on) }
        }

        @JavascriptInterface
        fun notificationsEnabled(): Boolean = Store.notificationsEnabled(ctx)

        @JavascriptInterface
        fun setActiveThread(serverId: String, threadId: String, provider: String) {
 UiState.activeProvider = provider
            UiState.activeServer = serverId
            UiState.activeThread = threadId
            if (serverId.isNotEmpty() && threadId.isNotEmpty()) {
                RelayWatchService.cancelThreadNotification(ctx, serverId, threadId, provider)
            }
        }

        @JavascriptInterface
        fun vibrate(ms: Int) {
            val v = if (Build.VERSION.SDK_INT >= 31) {
                (getSystemService(VibratorManager::class.java)).defaultVibrator
            } else {
                @Suppress("DEPRECATION")
                getSystemService(Vibrator::class.java)
            }
            v?.vibrate(
                VibrationEffect.createOneShot(ms.coerceIn(1, 5000).toLong(), VibrationEffect.DEFAULT_AMPLITUDE)
            )
        }

        @JavascriptInterface
        fun share(text: String) {
            val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text)
            runOnUiThread { startActivity(Intent.createChooser(send, null)) }
        }

        @JavascriptInterface
        fun copy(text: String) {
            runOnUiThread {
                getSystemService(ClipboardManager::class.java)
                    ?.setPrimaryClip(ClipData.newPlainText("fastcontrol", text))
            }
        }

        @JavascriptInterface
        fun consumeLaunchIntent(): String {
            synchronized(this@MainActivity) {
                val out = pendingLaunch
                pendingLaunch = ""
                return out
            }
        }

        @JavascriptInterface
        fun appUpdateState(): String = AppUpdates.snapshot

        @JavascriptInterface
        fun checkAppUpdates(manual: Boolean) = runOnUiThread { AppUpdates.checkForUpdates(manual) }

        @JavascriptInterface
        fun downloadAppUpdate() = runOnUiThread { AppUpdates.download() }

        @JavascriptInterface
        fun installAppUpdate() = runOnUiThread { requestAppInstall() }

        @JavascriptInterface
        fun deferAppUpdate() = runOnUiThread { pendingAppInstall = false; AppUpdates.later() }

        @JavascriptInterface
        fun appVersion(): String =
            packageManager.getPackageInfo(packageName, 0).versionName ?: ""
    }
}
