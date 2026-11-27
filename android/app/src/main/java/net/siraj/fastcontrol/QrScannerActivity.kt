package net.siraj.fastcontrol

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Canvas
import android.graphics.Color
import android.graphics.ImageFormat
import android.graphics.Matrix
import android.graphics.Paint
import android.graphics.PorterDuff
import android.graphics.PorterDuffXfermode
import android.graphics.RectF
import android.graphics.SurfaceTexture
import android.graphics.drawable.GradientDrawable
import android.hardware.camera2.CameraCaptureSession
import android.hardware.camera2.CameraCharacteristics
import android.hardware.camera2.CameraDevice
import android.hardware.camera2.CameraManager
import android.hardware.camera2.CaptureRequest
import android.media.ImageReader
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.HandlerThread
import android.os.VibrationEffect
import android.os.Vibrator
import android.os.VibratorManager
import android.util.Size
import android.util.TypedValue
import android.view.Gravity
import android.view.Surface
import android.view.TextureView
import android.view.View
import android.view.ViewGroup
import android.view.WindowInsets
import android.view.WindowInsetsController
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.TextView
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.math.abs
import kotlin.math.min

class QrScannerActivity : Activity() {

    companion object {
        const val EXTRA_PAIRING_JSON = "pairing_json"
        const val EXTRA_ERROR_MESSAGE = "error_message"
    }

    private lateinit var textureView: TextureView
    private lateinit var statusView: TextView
    private lateinit var flashButton: TextView
    private lateinit var topBar: LinearLayout
    private lateinit var bottomBar: LinearLayout

    private var cameraThread: HandlerThread? = null
    private var cameraHandler: Handler? = null
    private var decodeThread: HandlerThread? = null
    private var decodeHandler: Handler? = null

    private var cameraDevice: CameraDevice? = null
    private var captureSession: CameraCaptureSession? = null
    private var imageReader: ImageReader? = null
    private var previewSize: Size = Size(1280, 720)
    private var sensorOrientation: Int = 90
    private var cameraId: String? = null
    private var hasFlash: Boolean = false
    private var torchEnabled: Boolean = false

    private val decoding = AtomicBoolean(false)
    private val completed = AtomicBoolean(false)
    private var lastHintUpdateMs: Long = 0L

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        if (checkSelfPermission(Manifest.permission.CAMERA) != PackageManager.PERMISSION_GRANTED) {
            finishWithError("Allow Camera Access To Scan Pairing QR Codes")
            return
        }

        val root = FrameLayout(this).apply {
            setBackgroundColor(Color.BLACK)
        }

        textureView = TextureView(this).apply {
            layoutParams = FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.MATCH_PARENT,
            )
        }
        root.addView(textureView)

        val overlay = ScanReticleOverlay(this).apply {
            layoutParams = FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.MATCH_PARENT,
            )
        }
        root.addView(overlay)

        val dp = resources.displayMetrics.density
        topBar = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setPadding((16 * dp).toInt(), (16 * dp).toInt(), (16 * dp).toInt(), (12 * dp).toInt())
            layoutParams = FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
                Gravity.TOP,
            )
        }
        val titleView = TextView(this).apply {
            text = "Scan Pairing QR"
            setTextColor(Color.WHITE)
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 19f)
            typeface = android.graphics.Typeface.DEFAULT_BOLD
            layoutParams = LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f)
        }
        val closeButton = makePillButton("Close", dp) {
            setResult(RESULT_CANCELED)
            finish()
        }
        topBar.addView(titleView)
        topBar.addView(closeButton)
        root.addView(topBar)

        bottomBar = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER_HORIZONTAL
            setPadding((20 * dp).toInt(), (12 * dp).toInt(), (20 * dp).toInt(), (28 * dp).toInt())
            layoutParams = FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
                Gravity.BOTTOM,
            )
        }
        statusView = TextView(this).apply {
            text = "Point Camera At The Pairing QR Code"
            setTextColor(Color.WHITE)
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 14.5f)
            gravity = Gravity.CENTER
            setPadding((16 * dp).toInt(), (10 * dp).toInt(), (16 * dp).toInt(), (10 * dp).toInt())
            background = GradientDrawable().apply {
                setColor(Color.argb(178, 18, 18, 22))
                cornerRadius = 18 * dp
            }
            layoutParams = LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
            )
        }
        flashButton = makePillButton("Flash Off", dp) { toggleTorch() }.apply {
            visibility = View.GONE
            layoutParams = LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
            ).apply {
                topMargin = (14 * dp).toInt()
            }
        }
        bottomBar.addView(statusView)
        bottomBar.addView(flashButton)
        root.addView(bottomBar)

        setContentView(root)
        setupEdgeToEdge(root, dp)
    }

    private fun makePillButton(label: String, dp: Float, onClick: () -> Unit): TextView =
        TextView(this).apply {
            text = label
            setTextColor(Color.WHITE)
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 14f)
            typeface = android.graphics.Typeface.DEFAULT_BOLD
            gravity = Gravity.CENTER
            minHeight = (44 * dp).toInt()
            setPadding((18 * dp).toInt(), (10 * dp).toInt(), (18 * dp).toInt(), (10 * dp).toInt())
            background = GradientDrawable().apply {
                setColor(Color.argb(190, 32, 34, 40))
                setStroke((1 * dp).toInt().coerceAtLeast(1), Color.argb(90, 255, 255, 255))
                cornerRadius = 22 * dp
            }
            setOnClickListener { onClick() }
        }

    @Suppress("DEPRECATION")
    private fun setupEdgeToEdge(root: View, dp: Float) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            window.setDecorFitsSystemWindows(false)
            window.insetsController?.setSystemBarsAppearance(0, WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS or WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS)
        } else {
            window.decorView.systemUiVisibility =
                View.SYSTEM_UI_FLAG_LAYOUT_STABLE or View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN or View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION
        }
        if (Build.VERSION.SDK_INT < 35) {
            window.statusBarColor = Color.TRANSPARENT
            window.navigationBarColor = Color.TRANSPARENT
        }
        root.setOnApplyWindowInsetsListener { _, insets ->
            val topInset: Int
            val bottomInset: Int
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                val sys = insets.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout())
                topInset = sys.top
                bottomInset = sys.bottom
            } else {
                topInset = insets.systemWindowInsetTop
                bottomInset = insets.systemWindowInsetBottom
            }
            topBar.setPadding((16 * dp).toInt(), topInset + (12 * dp).toInt(), (16 * dp).toInt(), (12 * dp).toInt())
            bottomBar.setPadding((20 * dp).toInt(), (12 * dp).toInt(), (20 * dp).toInt(), bottomInset + (24 * dp).toInt())
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) WindowInsets.CONSUMED else insets.consumeSystemWindowInsets()
        }
        root.requestApplyInsets()
    }

    override fun onResume() {
        super.onResume()
        startThreads()
        if (textureView.isAvailable) {
            openCamera(textureView.width, textureView.height)
        } else {
            textureView.surfaceTextureListener = surfaceListener
        }
    }

    override fun onPause() {
        closeCamera()
        stopThreads()
        super.onPause()
    }

    private val surfaceListener = object : TextureView.SurfaceTextureListener {
        override fun onSurfaceTextureAvailable(surface: SurfaceTexture, width: Int, height: Int) {
            openCamera(width, height)
        }

        override fun onSurfaceTextureSizeChanged(surface: SurfaceTexture, width: Int, height: Int) {
            configureTransform(width, height)
        }

        override fun onSurfaceTextureDestroyed(surface: SurfaceTexture): Boolean = true

        override fun onSurfaceTextureUpdated(surface: SurfaceTexture) = Unit
    }

    private fun startThreads() {
        if (cameraThread == null) {
            cameraThread = HandlerThread("QrCamera").apply { start() }
            cameraHandler = Handler(cameraThread!!.looper)
        }
        if (decodeThread == null) {
            decodeThread = HandlerThread("QrDecode").apply { start() }
            decodeHandler = Handler(decodeThread!!.looper)
        }
    }

    private fun stopThreads() {
        cameraThread?.quitSafely()
        runCatching { cameraThread?.join(500) }
        cameraThread = null
        cameraHandler = null

        decodeThread?.quitSafely()
        runCatching { decodeThread?.join(500) }
        decodeThread = null
        decodeHandler = null
    }

    @SuppressLint("MissingPermission")
    private fun openCamera(viewWidth: Int, viewHeight: Int) {
        if (completed.get() || cameraDevice != null) return
        val manager = getSystemService(Context.CAMERA_SERVICE) as? CameraManager
        if (manager == null) {
            finishWithError("Camera Is Unavailable On This Device")
            return
        }
        val selected = selectCamera(manager)
        if (selected == null) {
            finishWithError("No Suitable Camera Found")
            return
        }
        cameraId = selected.first
        val chars = selected.second
        sensorOrientation = chars.get(CameraCharacteristics.SENSOR_ORIENTATION) ?: 90
        hasFlash = chars.get(CameraCharacteristics.FLASH_INFO_AVAILABLE) == true
        runOnUiThread {
            flashButton.visibility = if (hasFlash) View.VISIBLE else View.GONE
        }

        val map = chars.get(CameraCharacteristics.SCALER_STREAM_CONFIGURATION_MAP)
        val previewChoices = map?.getOutputSizes(SurfaceTexture::class.java)?.toList().orEmpty()
        previewSize = chooseBestSize(previewChoices, 1280, 720)
        val analysisChoices = map?.getOutputSizes(ImageFormat.YUV_420_888)?.toList().orEmpty()
        val analysisSize = chooseBestSize(analysisChoices, 1280, 720)

        configureTransform(viewWidth, viewHeight)

        imageReader?.close()
        imageReader = ImageReader.newInstance(analysisSize.width, analysisSize.height, ImageFormat.YUV_420_888, 2).apply {
            setOnImageAvailableListener({ reader ->
                val image = runCatching { reader.acquireLatestImage() }.getOrNull() ?: return@setOnImageAvailableListener
                if (completed.get() || !decoding.compareAndSet(false, true)) {
                    image.close()
                    return@setOnImageAvailableListener
                }
                val width = image.width
                val height = image.height
                val plane = image.planes[0]
                val rowStride = plane.rowStride
                val buffer = plane.buffer
                val yBytes = ByteArray(buffer.remaining())
                buffer.get(yBytes)
                image.close()

                val handler = decodeHandler
                if (handler == null) {
                    decoding.set(false)
                    return@setOnImageAvailableListener
                }
                handler.post {
                    try {
                        val decoded = QrPairingParser.decodeQrFromLuminance(yBytes, width, height, rowStride)
                        if (decoded != null) {
                            val parsed = QrPairingParser.parsePairingQr(decoded)
                            if (parsed != null) {
                                if (completed.compareAndSet(false, true)) {
                                    buzzSuccess()
                                    val resultIntent = Intent().putExtra(EXTRA_PAIRING_JSON, parsed.toJson().toString())
                                    runOnUiThread {
                                        setResult(RESULT_OK, resultIntent)
                                        finish()
                                    }
                                }
                            } else {
                                showInvalidHint()
                            }
                        }
                    } finally {
                        decoding.set(false)
                    }
                }
            }, cameraHandler)
        }

        try {
            manager.openCamera(cameraId!!, object : CameraDevice.StateCallback() {
                override fun onOpened(camera: CameraDevice) {
                    cameraDevice = camera
                    createCaptureSession(camera)
                }

                override fun onDisconnected(camera: CameraDevice) {
                    camera.close()
                    if (cameraDevice == camera) cameraDevice = null
                }

                override fun onError(camera: CameraDevice, error: Int) {
                    camera.close()
                    if (cameraDevice == camera) cameraDevice = null
                    finishWithError("Could Not Open Camera")
                }
            }, cameraHandler)
        } catch (_: Exception) {
            finishWithError("Could Not Open Camera")
        }
    }

    private fun selectCamera(manager: CameraManager): Pair<String, CameraCharacteristics>? {
        var fallback: Pair<String, CameraCharacteristics>? = null
        for (id in manager.cameraIdList) {
            val chars = runCatching { manager.getCameraCharacteristics(id) }.getOrNull() ?: continue
            val facing = chars.get(CameraCharacteristics.LENS_FACING)
            if (facing == CameraCharacteristics.LENS_FACING_BACK) return id to chars
            if (fallback == null) fallback = id to chars
        }
        return fallback
    }

    private fun chooseBestSize(choices: List<Size>, targetW: Int, targetH: Int): Size {
        if (choices.isEmpty()) return Size(targetW, targetH)
        val reasonable = choices.filter { it.width in 640..1920 && it.height in 480..1080 }
        val pool = if (reasonable.isNotEmpty()) reasonable else choices
        return pool.minByOrNull { abs(it.width - targetW) + abs(it.height - targetH) } ?: choices[0]
    }

    @Suppress("DEPRECATION")
    private fun createCaptureSession(camera: CameraDevice) {
        val surfaceTexture = textureView.surfaceTexture ?: return
        surfaceTexture.setDefaultBufferSize(previewSize.width, previewSize.height)
        val previewSurface = Surface(surfaceTexture)
        val readerSurface = imageReader?.surface ?: return

        try {
            camera.createCaptureSession(
                listOf(previewSurface, readerSurface),
                object : CameraCaptureSession.StateCallback() {
                    override fun onConfigured(session: CameraCaptureSession) {
                        if (cameraDevice == null || completed.get()) return
                        captureSession = session
                        applyRepeatingRequest(session, camera, previewSurface, readerSurface)
                    }

                    override fun onConfigureFailed(session: CameraCaptureSession) {
                        finishWithError("Could Not Start Camera Preview")
                    }
                },
                cameraHandler,
            )
        } catch (_: Exception) {
            finishWithError("Could Not Start Camera Preview")
        }
    }

    private fun applyRepeatingRequest(
        session: CameraCaptureSession,
        camera: CameraDevice,
        previewSurface: Surface,
        readerSurface: Surface,
    ) {
        try {
            val builder = camera.createCaptureRequest(CameraDevice.TEMPLATE_PREVIEW).apply {
                addTarget(previewSurface)
                addTarget(readerSurface)
                set(CaptureRequest.CONTROL_AF_MODE, CaptureRequest.CONTROL_AF_MODE_CONTINUOUS_PICTURE)
                set(CaptureRequest.CONTROL_AE_MODE, CaptureRequest.CONTROL_AE_MODE_ON)
                if (hasFlash) {
                    set(
                        CaptureRequest.FLASH_MODE,
                        if (torchEnabled) CaptureRequest.FLASH_MODE_TORCH else CaptureRequest.FLASH_MODE_OFF,
                    )
                }
            }
            session.setRepeatingRequest(builder.build(), null, cameraHandler)
        } catch (_: Exception) {
        }
    }

    private fun toggleTorch() {
        if (!hasFlash) return
        val session = captureSession ?: return
        val camera = cameraDevice ?: return
        val surfaceTexture = textureView.surfaceTexture ?: return
        val readerSurface = imageReader?.surface ?: return
        torchEnabled = !torchEnabled
        flashButton.text = if (torchEnabled) "Flash On" else "Flash Off"
        applyRepeatingRequest(session, camera, Surface(surfaceTexture), readerSurface)
    }

    @Suppress("DEPRECATION")
    private fun configureTransform(viewWidth: Int, viewHeight: Int) {
        if (viewWidth <= 0 || viewHeight <= 0) return
        val rotation = windowManager.defaultDisplay?.rotation ?: Surface.ROTATION_0
        val matrix = Matrix()
        val viewRect = RectF(0f, 0f, viewWidth.toFloat(), viewHeight.toFloat())
        val bufferRect = RectF(0f, 0f, previewSize.height.toFloat(), previewSize.width.toFloat())
        val centerX = viewRect.centerX()
        val centerY = viewRect.centerY()

        if (rotation == Surface.ROTATION_90 || rotation == Surface.ROTATION_270) {
            bufferRect.offset(centerX - bufferRect.centerX(), centerY - bufferRect.centerY())
            matrix.setRectToRect(viewRect, bufferRect, Matrix.ScaleToFit.FILL)
            val scale = maxOf(
                viewHeight.toFloat() / previewSize.height,
                viewWidth.toFloat() / previewSize.width,
            )
            matrix.postScale(scale, scale, centerX, centerY)
            matrix.postRotate((90 * (rotation - 2)).toFloat(), centerX, centerY)
        } else {
            val previewRatio = previewSize.height.toFloat() / previewSize.width.toFloat()
            val viewRatio = viewWidth.toFloat() / viewHeight.toFloat()
            val scaleX: Float
            val scaleY: Float
            if (viewRatio > previewRatio) {
                scaleX = 1f
                scaleY = viewRatio / previewRatio
            } else {
                scaleX = previewRatio / viewRatio
                scaleY = 1f
            }
            matrix.setScale(scaleX, scaleY, centerX, centerY)
            if (rotation == Surface.ROTATION_180) {
                matrix.postRotate(180f, centerX, centerY)
            }
        }
        runOnUiThread { textureView.setTransform(matrix) }
    }

    private fun showInvalidHint() {
        val now = android.os.SystemClock.elapsedRealtime()
        if (now - lastHintUpdateMs < 1500L) return
        lastHintUpdateMs = now
        runOnUiThread {
            if (!isDestroyed && !completed.get()) {
                statusView.text = "Not A fastcontrol Pairing QR"
            }
        }
    }

    private fun buzzSuccess() {
        val vibrator = if (Build.VERSION.SDK_INT >= 31) {
            getSystemService(VibratorManager::class.java)?.defaultVibrator
        } else {
            @Suppress("DEPRECATION")
            getSystemService(Vibrator::class.java)
        }
        runCatching {
            vibrator?.vibrate(VibrationEffect.createOneShot(25L, VibrationEffect.DEFAULT_AMPLITUDE))
        }
    }

    private fun closeCamera() {
        runCatching { captureSession?.close() }
        captureSession = null
        runCatching { cameraDevice?.close() }
        cameraDevice = null
        runCatching { imageReader?.close() }
        imageReader = null
    }

    private fun finishWithError(message: String) {
        if (!completed.compareAndSet(false, true)) return
        runOnUiThread {
            setResult(RESULT_CANCELED, Intent().putExtra(EXTRA_ERROR_MESSAGE, message))
            finish()
        }
    }

    private class ScanReticleOverlay(context: Context) : View(context) {
        private val scrimPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply {
            color = Color.argb(145, 0, 0, 0)
        }
        private val clearPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply {
            xfermode = PorterDuffXfermode(PorterDuff.Mode.CLEAR)
        }
        private val borderPaint = Paint(Paint.ANTI_ALIAS_FLAG).apply {
            color = Color.argb(235, 255, 255, 255)
            style = Paint.Style.STROKE
            strokeWidth = 3f * resources.displayMetrics.density
            strokeCap = Paint.Cap.ROUND
        }

        init {
            setLayerType(LAYER_TYPE_SOFTWARE, null)
        }

        override fun onDraw(canvas: Canvas) {
            super.onDraw(canvas)
            val dp = resources.displayMetrics.density
            val side = min(width, height) * 0.68f
            val left = (width - side) / 2f
            val top = (height - side) / 2f
            val rect = RectF(left, top, left + side, top + side)
            val radius = 22f * dp

            canvas.drawRect(0f, 0f, width.toFloat(), height.toFloat(), scrimPaint)
            canvas.drawRoundRect(rect, radius, radius, clearPaint)

            val corner = 28f * dp
            // Top-left
            canvas.drawLine(left, top + radius, left, top + radius + corner, borderPaint)
            canvas.drawLine(left + radius, top, left + radius + corner, top, borderPaint)
            // Top-right
            canvas.drawLine(rect.right, top + radius, rect.right, top + radius + corner, borderPaint)
            canvas.drawLine(rect.right - radius, top, rect.right - radius - corner, top, borderPaint)
            // Bottom-left
            canvas.drawLine(left, rect.bottom - radius, left, rect.bottom - radius - corner, borderPaint)
            canvas.drawLine(left + radius, rect.bottom, left + radius + corner, rect.bottom, borderPaint)
            // Bottom-right
            canvas.drawLine(rect.right, rect.bottom - radius, rect.right, rect.bottom - radius - corner, borderPaint)
            canvas.drawLine(rect.right - radius, rect.bottom, rect.right - radius - corner, rect.bottom, borderPaint)
        }
    }
}
