package net.siraj.fastcontrol

import okhttp3.OkHttpClient
import java.security.MessageDigest
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.util.concurrent.ConcurrentHashMap
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocketFactory
import javax.net.ssl.X509TrustManager

/** A paired public key is the trust anchor. OkHttp still verifies dates and hostnames. */
object RelayTLS {
    private data class Trust(val factory: SSLSocketFactory, val manager: X509TrustManager)
    private val identities = ConcurrentHashMap<String, Trust>()
    fun validPin(pin: String) = pin.matches(Regex("[a-f0-9]{64}"))
    fun reset() = identities.clear()

    fun client(server: Server?, base: OkHttpClient): OkHttpClient {
        val pin = server?.pin.orEmpty()
        if (pin.isEmpty()) return base
        check(validPin(pin) && server!!.url.startsWith("https://")) { "Pair this computer again" }
        val trust = identities.computeIfAbsent(pin) {
            val expected = ByteArray(32) { index -> pin.substring(index * 2, index * 2 + 2).toInt(16).toByte() }
            val manager = object : X509TrustManager {
                override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
                override fun checkClientTrusted(chain: Array<X509Certificate>, authType: String) {
                    throw CertificateException("Client certificates are not accepted")
                }
                override fun checkServerTrusted(chain: Array<X509Certificate>, authType: String) {
                    val leaf = chain.firstOrNull() ?: throw CertificateException("Missing server certificate")
                    leaf.checkValidity()
                    val actual = MessageDigest.getInstance("SHA-256").digest(leaf.publicKey.encoded)
                    if (!MessageDigest.isEqual(expected, actual)) throw CertificateException("Computer identity changed. Pair it again")
                }
            }
            val context = SSLContext.getInstance("TLS")
            context.init(null, arrayOf(manager), null)
            Trust(context.socketFactory, manager)
        }
        return base.newBuilder().sslSocketFactory(trust.factory, trust.manager)
            .followRedirects(false).followSslRedirects(false).build()
    }
}
