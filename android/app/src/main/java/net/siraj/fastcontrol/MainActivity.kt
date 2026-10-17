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
    }

    private lateinit var container: FrameLayout
    private lateinit var webView: WebView
    private var fileCallback: ValueCallback<Array<Uri>>? = null
    private lateinit var voice: VoiceRecorder
    private var awaitingMic = false
    private var voiceRequestId = ""
    private var voiceCompressed = false
    private lateinit var downloads: Downloads
    private lateinit var uploads: ImageUploads
    private lateinit var audioStreams: AudioStreams
    private var pendingImage: File? = null

    @Volatile private var pendingLaunch = ""

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        if ((applicationInfo.flags and ApplicationInfo.FLAG_DEBUGGABLE) != 0) {
            WebView.setWebContentsDebuggingEnabled(true)
        }

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
        voice = VoiceRecorder(this, { runOnUiThread { updateVoiceScreenState() } }) { event ->
            runOnUiThread { if (!isDestroyed) webView.evaluateJavascript("window.onVoiceEvent && window.onVoiceEvent($event)", null) }
        }
        uploads = ImageUploads { event -> runOnUiThread {
            if (!isDestroyed) webView.evaluateJavascript("window.onImageUpload && window.onImageUpload($event)", null)
        } }
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
        // A force stop or app update kills the service; bring it back if the user wants alerts.
        if (Store.notificationsEnabled(this) && !RelayWatchService.isRunning) {
            startForegroundService(Intent(this, RelayWatchService::class.java))
        }
        webView.onResume()
        updateVoiceScreenState()
    }

    override fun onPause() {
        UiState.resumed = false
        webView.evaluateJavascript("window.pauseChatAudio && window.pauseChatAudio()", null)
        if (::voice.isInitialized) voice.cancel()
        updateVoiceScreenState()
        webView.onPause()
        super.onPause()
    }

    override fun onDestroy() {
        if (::voice.isInitialized) voice.cancel()
        pendingImage?.delete(); pendingImage = null
        webView.destroy()
        super.onDestroy()
    }

    private fun updateVoiceScreenState() {
        if (isDestroyed) return
        // Read the current session on the UI thread so an old worker cannot clear a new recording's flag.
        val recording = UiState.resumed && ::voice.isInitialized && voice.isRecording
        if (recording) window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        else window.clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
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

    private fun openExternal(uri: Uri) {
        try {
            startActivity(Intent(Intent.ACTION_VIEW, uri).addCategory(Intent.CATEGORY_BROWSABLE))
        } catch (_: ActivityNotFoundException) {
        }
    }

    private inner class ShellClient : WebViewClient() {
        override fun shouldInterceptRequest(
            view: WebView, request: WebResourceRequest
        ): WebResourceResponse? {
            val url = request.url
            if (url.host != ASSET_HOST) return null
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
            if (grantResults.firstOrNull() == PackageManager.PERMISSION_GRANTED) voice.start(voiceRequestId, voiceCompressed)
            else webView.evaluateJavascript("window.onVoiceEvent && window.onVoiceEvent(${JSONObject().put("type", "error").put("id", voiceRequestId).put("message", "Microphone Permission Is Required")})", null)
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
            downloads.download(url, server.token, path.replace('\\', '/').substringAfterLast('/'))
        }

        @JavascriptInterface
        fun downloadRemoteFile(url: String, name: String) {
            if (Uri.parse(url).scheme in setOf("https", "http")) downloads.download(url, fileName = name)
        }

        @JavascriptInterface
        fun getServers(): String = Store.serversJson(ctx)

        @JavascriptInterface
        fun downloadImage(serverId: String, base: String, path: String) {
            val server = Store.servers(ctx).firstOrNull { it.id == serverId && base.trimEnd('/') in it.urls.map { u -> u.trimEnd('/') } } ?: return
            val url = base.trimEnd('/') + "/image?path=" + java.net.URLEncoder.encode(path, "UTF-8")
            downloads.download(url, server.token)
        }

        @JavascriptInterface
        fun downloadRemoteImage(url: String) { if (Uri.parse(url).scheme == "https") downloads.download(url) }

        @JavascriptInterface
        fun uploadImage(requestId: String, serverId: String, base: String, mime: String, encoded: String) {
            val server = Store.servers(ctx).firstOrNull { it.id == serverId }
            uploads.upload(requestId, server, base, mime, encoded)
        }

        @JavascriptInterface
        fun startVoice(requestId: String) = startVoiceRequest(requestId, false)

        @JavascriptInterface
        fun startVoiceCompressed(requestId: String) = startVoiceRequest(requestId, true)

        private fun startVoiceRequest(requestId: String, compressed: Boolean) = runOnUiThread {
            voiceRequestId = requestId; voiceCompressed = compressed
            if (checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) voice.start(requestId, compressed)
            else { awaitingMic = true; requestPermissions(arrayOf(Manifest.permission.RECORD_AUDIO), REQ_MIC) }
        }

        @JavascriptInterface
        fun stopVoice(serverId: String, base: String) {
            val server = Store.servers(ctx).firstOrNull { it.id == serverId && base.trimEnd('/') in it.urls.map { u -> u.trimEnd('/') } } ?: return
            voice.finish(server, base)
        }

        @JavascriptInterface
        fun cancelVoice() = runOnUiThread { awaitingMic = false; voice.cancel() }

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
        fun appVersion(): String =
            packageManager.getPackageInfo(packageName, 0).versionName ?: ""
    }
}
