// «Мост» для Android: тонкая оболочка (Java, без сторонних библиотек) вокруг ядра most.aar (gomobile).
import java.util.Properties

plugins {
    id("com.android.application")
}

val signProps = Properties().apply {
    val f = rootProject.file("signing.properties") // вне git; пути к ключу подписи
    if (f.exists()) f.inputStream().use { load(it) }
}

android {
    namespace = "ru.most.app"
    compileSdk = 35
    defaultConfig {
        applicationId = "ru.most.app"
        minSdk = 26
        targetSdk = 35
        versionCode = (project.findProperty("mostVersionCode") as String?)?.toInt() ?: 1
        versionName = (project.findProperty("mostVersionName") as String?) ?: "0.8.0"
        ndk { abiFilters += listOf("arm64-v8a", "armeabi-v7a", "x86_64") }
    }
    signingConfigs {
        create("release") {
            if (signProps.isNotEmpty()) {
                storeFile = file(signProps.getProperty("storeFile"))
                storePassword = signProps.getProperty("storePassword")
                keyAlias = signProps.getProperty("keyAlias")
                keyPassword = signProps.getProperty("keyPassword")
            }
        }
    }
    buildTypes {
        release {
            isMinifyEnabled = false
            signingConfig = signingConfigs.getByName("release")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

dependencies {
    implementation(files("libs/most.aar"))
}
