package net.siraj.fastcontrol

import android.annotation.SuppressLint
import android.content.Context
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import okhttp3.Call
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.asRequestBody
import org.json.JSONObject
import java.io.File
import java.io.RandomAccessFile
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.util.concurrent.TimeUnit
import kotlin.math.sqrt

/** Captures audio on one worker and uploads after the user stops recording. */
class VoiceRecorder(
    private val context: Context,
    private val recordingChanged: () -> Unit,
    private val emit: (JSONObject) -> Unit
) {
    private class Session(val id: String, val compressed: Boolean) {
        @Volatile var stopped = false
        @Volatile var cancelled = false
        @Volatile var server: Server? = null
        @Volatile var base = ""
        @Volatile var call: Call? = null
    }
    private val lock = Any()
    private var current: Session? = null
    val isBusy: Boolean get() = synchronized(lock) { current != null }
    val isRecording: Boolean
        get() = synchronized(lock) { current?.let { !it.stopped && !it.cancelled } == true }
    private val client = OkHttpClient.Builder().connectTimeout(5, TimeUnit.SECONDS)
        .readTimeout(100, TimeUnit.SECONDS).callTimeout(105, TimeUnit.SECONDS).build()

    private fun event(s: Session, type: String, extra: JSONObject = JSONObject()) {
        synchronized(lock) {
            if (current !== s || s.cancelled) return
            emit(extra.put("type", type).put("id", s.id))
        }
    }

    @SuppressLint("MissingPermission")
    fun start(id: String, compressed: Boolean = false) {
        val s = synchronized(lock) {
            if (current != null) return
            Session(id, compressed).also { current = it }
        }
        recordingChanged()
        Thread({
            var file: File? = null
            var recorder: AudioRecord? = null
            try {
                if (s.compressed) {
                    file = File.createTempFile("voice-", ".m4a", context.cacheDir)
                    captureAac(s, file)
                } else {
                    val min = AudioRecord.getMinBufferSize(16000, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT)
                    check(min > 0) { "Microphone Is Unavailable" }
                    val audio = AudioRecord(MediaRecorder.AudioSource.VOICE_RECOGNITION, 16000,
                        AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT, maxOf(min, 8192))
                    recorder = audio
                    check(audio.state == AudioRecord.STATE_INITIALIZED) { "Microphone Is Unavailable" }
                    file = File.createTempFile("voice-", ".wav", context.cacheDir)
                    var bytes = 0
                    RandomAccessFile(file, "rw").use { out ->
                        out.write(ByteArray(44))
                        audio.startRecording()
                        check(audio.recordingState == AudioRecord.RECORDSTATE_RECORDING) { "Could Not Start Recording" }
                        event(s, "recording")
                        val samples = ShortArray(800)
                        val buffer = ByteBuffer.allocate(1600).order(ByteOrder.LITTLE_ENDIAN)
                        while (!s.stopped && bytes < 16000 * 2 * 120) {
                            val count = audio.read(samples, 0, samples.size)
                            check(count > 0) { "Microphone Recording Failed" }
                            buffer.clear()
                            var energy = 0.0
                            for (i in 0 until count) { buffer.putShort(samples[i]); val v = samples[i] / 32768.0; energy += v * v }
                            out.write(buffer.array(), 0, count * 2)
                            bytes += count * 2
                            event(s, "level", JSONObject().put("level", sqrt(energy / count)).put("seconds", bytes / 32000.0))
                        }
                        audio.stop(); audio.release(); recorder = null
                        out.seek(0); out.write(wavHeader(bytes))
                    }
                }
                s.stopped = true
                recordingChanged()
                if (s.cancelled) return@Thread
                val server = s.server
                if (server == null) {
                    event(s, "error", JSONObject().put("message", "Recording Limit Reached. Try a Shorter Message"))
                    return@Thread
                }
                check(if (s.compressed) file.length() >= 100 else file.length() >= 3244) { "Record a Little Longer" }
                event(s, "transcribing")
                val started = System.nanoTime()
                val request = Request.Builder().url(s.base.trimEnd('/') + "/transcribe")
                    .header("Authorization", "Bearer " + server.token)
                    .post(file.asRequestBody((if (s.compressed) "audio/mp4" else "audio/wav").toMediaType())).build()
                val call = client.newCall(request); s.call = call
                if (s.cancelled) { call.cancel(); return@Thread }
                call.execute().use { response ->
                    val body = response.body?.string() ?: ""
                    val data = try { JSONObject(body) } catch (_: Exception) { JSONObject() }
                    check(response.isSuccessful) { data.optString("error").ifEmpty { "Could Not Transcribe On The Mac" } }
                    val text = data.optString("text").trim()
                    check(text.isNotEmpty()) { "No Speech Detected" }
                    event(s, "transcript", JSONObject().put("text", text).put("durationMs", data.optLong("durationMs"))
                        .put("totalMs", (System.nanoTime() - started) / 1000000)
                        .put("audioBytes", data.optLong("audioBytes", file.length())).put("uploadBytes", file.length()))
                }
            } catch (e: Exception) {
                event(s, "error", JSONObject().put("message", e.message ?: "Voice Recording Failed"))
            } finally {
                s.stopped = true
                recordingChanged()
                try {
                    try { recorder?.stop() } catch (_: Exception) {}
                    recorder?.release()
                    file?.delete()
                } finally {
                    synchronized(lock) { if (current === s) current = null }
                    recordingChanged()
                }
            }
        }, "RelayVoice").start()
    }

    @Suppress("DEPRECATION")
    @SuppressLint("MissingPermission")
    private fun captureAac(s: Session, file: File) {
        val audio = MediaRecorder()
        try {
            audio.setAudioSource(MediaRecorder.AudioSource.VOICE_RECOGNITION)
            audio.setOutputFormat(MediaRecorder.OutputFormat.MPEG_4)
            audio.setAudioEncoder(MediaRecorder.AudioEncoder.AAC)
            audio.setAudioSamplingRate(16000)
            audio.setAudioChannels(1)
            audio.setAudioEncodingBitRate(32000)
            audio.setOutputFile(file.absolutePath)
            audio.prepare(); audio.start()
            event(s, "recording")
            val started = System.nanoTime()
            while (!s.stopped && (System.nanoTime() - started) < 119500000000L) {
                event(s, "level", JSONObject().put("level", audio.maxAmplitude / 32768.0)
                    .put("seconds", (System.nanoTime() - started) / 1000000000.0))
                Thread.sleep(50)
            }
            audio.stop()
        } finally { audio.release() }
    }

    fun finish(server: Server, base: String) {
        synchronized(lock) { current?.let { it.server = server; it.base = base; it.stopped = true } }
        recordingChanged()
    }

    fun cancel() {
        synchronized(lock) { current?.let { it.cancelled = true; it.stopped = true; it.call?.cancel() } }
        recordingChanged()
    }

    private fun wavHeader(bytes: Int): ByteArray = ByteBuffer.allocate(44).order(ByteOrder.LITTLE_ENDIAN)
        .put("RIFF".toByteArray()).putInt(bytes + 36).put("WAVEfmt ".toByteArray()).putInt(16)
        .putShort(1).putShort(1).putInt(16000).putInt(32000).putShort(2).putShort(16)
        .put("data".toByteArray()).putInt(bytes).array()
}
