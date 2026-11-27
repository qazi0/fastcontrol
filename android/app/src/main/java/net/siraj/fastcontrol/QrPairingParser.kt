package net.siraj.fastcontrol

import com.google.zxing.BinaryBitmap
import com.google.zxing.DecodeHintType
import com.google.zxing.PlanarYUVLuminanceSource
import com.google.zxing.common.GlobalHistogramBinarizer
import com.google.zxing.common.HybridBinarizer
import com.google.zxing.qrcode.QRCodeReader
import org.json.JSONArray
import org.json.JSONObject
import java.net.URI
import java.net.URLDecoder
import java.nio.charset.StandardCharsets

internal data class ParsedPairing(
    val url: String,
    val token: String,
    val name: String = "",
    val pin: String = "",
    val alts: List<String> = emptyList(),
) {
    fun toJson(): JSONObject = JSONObject()
        .put("ok", true)
        .put("url", url)
        .put("token", token)
        .put("name", name)
        .put("pin", pin)
        .put("alts", JSONArray(alts))
}

internal object QrPairingParser {
    private val pinRegex = Regex("^[a-fA-F0-9]{64}$")

    private val decodeHints: Map<DecodeHintType, Any> = mapOf(
        DecodeHintType.TRY_HARDER to true,
        DecodeHintType.ALSO_INVERTED to true,
        DecodeHintType.CHARACTER_SET to "UTF-8",
    )

    fun parsePairingQr(raw: String?): ParsedPairing? {
        val text = raw?.trim().orEmpty()
        if (text.isEmpty() || text.length > 4096) return null
        val uri = runCatching { URI(text) }.getOrNull() ?: return null
        val scheme = uri.scheme?.lowercase() ?: return null

        return when {
            scheme == "fastcontrol" && uri.host?.lowercase() == "pair" -> parseAppPairingUri(uri)
            scheme == "http" || scheme == "https" -> parseWebPairingUri(uri)
            else -> null
        }
    }

    private fun parseAppPairingUri(uri: URI): ParsedPairing? {
        val params = parseQueryParams(uri.rawQuery)
        val url = normalizeOrigin(params["url"]) ?: return null
        val token = params["t"]?.trim().orEmpty()
        if (token.isEmpty()) return null
        val rawPin = params["pin"]?.trim().orEmpty().lowercase()
        if (rawPin.isNotEmpty() && (!url.startsWith("https://") || !pinRegex.matches(rawPin))) {
            return null
        }
        val name = params["name"]?.trim().orEmpty()
        val alts = parseAlternates(params["alts"], primary = url, requireHttps = rawPin.isNotEmpty())
        return ParsedPairing(url = url, token = token, name = name, pin = rawPin, alts = alts)
    }

    private fun parseWebPairingUri(uri: URI): ParsedPairing? {
        val origin = normalizeOrigin(buildOrigin(uri)) ?: return null
        val fragment = uri.rawFragment ?: return null
        val params = parseQueryParams(fragment)
        val token = params["t"]?.trim().orEmpty()
        if (token.isEmpty()) return null
        val rawPin = params["pin"]?.trim().orEmpty().lowercase()
        if (rawPin.isNotEmpty() && (!origin.startsWith("https://") || !pinRegex.matches(rawPin))) {
            return null
        }
        return ParsedPairing(url = origin, token = token, name = "", pin = rawPin, alts = emptyList())
    }

    private fun parseQueryParams(raw: String?): Map<String, String> {
        if (raw.isNullOrEmpty()) return emptyMap()
        val out = LinkedHashMap<String, String>()
        for (part in raw.split('&')) {
            if (part.isEmpty()) continue
            val eq = part.indexOf('=')
            val rawKey = if (eq >= 0) part.substring(0, eq) else part
            val rawVal = if (eq >= 0) part.substring(eq + 1) else ""
            val key = decodeComponent(rawKey)
            if (key.isNotEmpty() && !out.containsKey(key)) {
                out[key] = decodeComponent(rawVal)
            }
        }
        return out
    }

    private fun decodeComponent(value: String): String =
        runCatching { URLDecoder.decode(value, StandardCharsets.UTF_8.name()) }.getOrDefault(value)

    private fun buildOrigin(uri: URI): String? {
        val scheme = uri.scheme?.lowercase() ?: return null
        val host = uri.host ?: return null
        if (!uri.userInfo.isNullOrEmpty()) return null
        val formattedHost = if (host.contains(':') && !host.startsWith("[")) "[$host]" else host
        val port = uri.port
        return if (port > 0) "$scheme://$formattedHost:$port" else "$scheme://$formattedHost"
    }

    private fun normalizeOrigin(raw: String?): String? {
        val trimmed = raw?.trim()?.trimEnd('/') ?: return null
        if (trimmed.isEmpty()) return null
        val parsed = runCatching { URI(trimmed) }.getOrNull() ?: return null
        val scheme = parsed.scheme?.lowercase() ?: return null
        if (scheme != "http" && scheme != "https") return null
        if (!parsed.userInfo.isNullOrEmpty() || !parsed.rawQuery.isNullOrEmpty() || !parsed.rawFragment.isNullOrEmpty()) {
            return null
        }
        val path = parsed.rawPath.orEmpty()
        if (path.isNotEmpty() && path != "/") return null
        return buildOrigin(parsed)
    }

    private fun parseAlternates(rawAlts: String?, primary: String, requireHttps: Boolean): List<String> {
        if (rawAlts.isNullOrBlank()) return emptyList()
        val arr = runCatching { JSONArray(rawAlts) }.getOrNull() ?: return emptyList()
        val out = ArrayList<String>()
        val seen = HashSet<String>()
        seen.add(primary)
        for (i in 0 until arr.length()) {
            val origin = normalizeOrigin(arr.optString(i)) ?: continue
            if (requireHttps && !origin.startsWith("https://")) continue
            if (seen.add(origin)) {
                out.add(origin)
                if (out.size >= 8) break
            }
        }
        return out
    }

    fun decodeQrFromLuminance(
        yBytes: ByteArray,
        width: Int,
        height: Int,
        rowStride: Int = width,
    ): String? {
        if (width <= 0 || height <= 0 || rowStride < width) return null
        val packed = if (rowStride == width && yBytes.size >= width * height) {
            yBytes
        } else {
            val out = ByteArray(width * height)
            for (row in 0 until height) {
                val srcOffset = row * rowStride
                if (srcOffset + width > yBytes.size) return null
                System.arraycopy(yBytes, srcOffset, out, row * width, width)
            }
            out
        }

        val source = PlanarYUVLuminanceSource(
            packed,
            width,
            height,
            0,
            0,
            width,
            height,
            false,
        )
        val inverted = source.invert()
        val reader = QRCodeReader()
        val candidates = arrayOf(
            BinaryBitmap(HybridBinarizer(source)),
            BinaryBitmap(HybridBinarizer(inverted)),
            BinaryBitmap(GlobalHistogramBinarizer(source)),
            BinaryBitmap(GlobalHistogramBinarizer(inverted)),
        )
        for (bitmap in candidates) {
            try {
                val result = reader.decode(bitmap, decodeHints)
                if (!result.text.isNullOrBlank()) return result.text
            } catch (_: Exception) {
            } finally {
                reader.reset()
            }
        }
        return null
    }
}
