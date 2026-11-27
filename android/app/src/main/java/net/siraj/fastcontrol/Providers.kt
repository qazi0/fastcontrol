package net.siraj.fastcontrol

/** Canonical provider identifiers, display labels, and notification defaults. */
object Providers {
    const val CODEX = "codex"
    const val CLAUDE = "claude"
    const val ANTIGRAVITY = "antigravity"

    /**
     * Normalizes an incoming provider identifier to one of the three supported providers.
     * Unknown or blank values fall back safely to `"codex"`.
     */
    fun normalize(raw: String?): String = when (raw?.trim()?.lowercase()) {
        CLAUDE -> CLAUDE
        ANTIGRAVITY -> ANTIGRAVITY
        CODEX -> CODEX
        else -> CODEX
    }

    /** User-visible Title Case display name for a provider. */
    fun displayName(raw: String?): String = when (normalize(raw)) {
        CLAUDE -> "Claude Code"
        ANTIGRAVITY -> "Antigravity"
        else -> "Codex"
    }

    /** Default Title Case notification title when an event omits a thread title. */
    fun fallbackNotificationTitle(raw: String?, kind: String): String {
        val label = displayName(raw)
        return when (kind.trim().lowercase()) {
            "attention" -> "$label Needs Attention"
            else -> label
        }
    }

    /** Default Title Case notification body when an event omits detail text. */
    fun fallbackNotificationText(kind: String, hasExplicitTitle: Boolean = true): String =
        when (kind.trim().lowercase()) {
            "failed" -> "Turn Failed"
            "attention" -> if (hasExplicitTitle) "Needs Your Attention" else "Permission Or Input Required"
            else -> "Turn Completed"
        }

    /** Formats the notification subtext combining server name and provider display label. */
    fun notificationSubText(serverName: String, raw: String?): String {
        val label = displayName(raw)
        val trimmedServer = serverName.trim()
        return if (trimmedServer.isEmpty()) label else "$trimmedServer · $label"
    }
}

internal data class ThreadDeepLink(
    val serverId: String,
    val threadId: String,
    val provider: String,
)
