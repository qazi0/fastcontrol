import org.jetbrains.kotlin.gradle.dsl.JvmTarget
import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// The web UI in ../web is the single source of truth. It is copied into a generated
// assets directory before every build, so the APK always bundles the current UI.
val webSrc = rootProject.file("../web")
val webAssetsRoot = layout.buildDirectory.dir("generated/webAssets")
val syncWebAssets by tasks.registering(Sync::class) {
    from(webSrc) {
        exclude("**/.*")
    }
    into(webAssetsRoot.map { it.dir("web") })
}
val signing = Properties().apply {
    val file = rootProject.file("signing.properties")
    if (file.exists()) file.inputStream().use { load(it) }
}

android {
    namespace = "net.siraj.fastcontrol"
    compileSdk = 36

    defaultConfig {
        applicationId = "net.siraj.fastcontrol"
        minSdk = 26
        targetSdk = 36
        versionCode = 107
        versionName = "0.1.7"
    }

    signingConfigs {
        if (signing.isNotEmpty()) create("distribution") {
            storeFile = file(signing.getProperty("storeFile"))
            storePassword = signing.getProperty("storePassword")
            keyAlias = signing.getProperty("keyAlias")
            keyPassword = signing.getProperty("keyPassword")
        }
    }
    buildTypes {
        release {
            isMinifyEnabled = false
            if (signing.isNotEmpty()) signingConfig = signingConfigs.getByName("distribution")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    sourceSets["main"].assets.srcDir(webAssetsRoot)
}

kotlin {
    compilerOptions {
        jvmTarget.set(JvmTarget.JVM_17)
    }
}

tasks.named("preBuild") { dependsOn(syncWebAssets) }
tasks.matching { it.name == "assembleRelease" }.configureEach {
    doFirst { check(signing.isNotEmpty()) { "Create android/signing.properties Before Building a Release APK" } }
}

dependencies {
    // kotlin-stdlib-jdk7/jdk8 are empty shims since Kotlin 1.8 and the versions OkHttp
    // asks for are not in the offline cache, so drop them; kotlin-stdlib covers it.
    implementation("com.squareup.okhttp3:okhttp:4.12.0") {
        exclude(group = "org.jetbrains.kotlin", module = "kotlin-stdlib-jdk8")
        exclude(group = "org.jetbrains.kotlin", module = "kotlin-stdlib-jdk7")
    }
}
