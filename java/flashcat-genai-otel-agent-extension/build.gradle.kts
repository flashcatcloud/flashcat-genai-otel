import java.util.zip.ZipFile

plugins {
    java
    `maven-publish`
}

description = "Flashcat GenAI OTel extension jar for the OpenTelemetry Java agent (-Dotel.javaagent.extensions=...)"

// This module's source contains only the openai-java InstrumentationModule and its helpers in cloud.flashcat.genai.agent.openai.
// The agent or application supplies the agent extension API, OTel API, and openai-java at runtime. The jar excludes them.
// Compile against 2.20.1, the oldest supported openai-java version. The methods used here exist in 2.x through 4.x.
// Integration tests cover both 2.x and 4.x.
tasks.named<JavaCompile>("compileJava") {
    options.release.set(8)
    // Disable -Xlint:classfile because Byte Buddy class files reference SpotBugs annotations absent from the classpath.
    options.compilerArgs.addAll(listOf("-Xlint:all,-options,-serial,-processing,-classfile", "-Werror"))
}
tasks.named<JavaCompile>("compileTestJava") { options.release.set(8) }

// Bundle only the flashcat-genai-otel jar without transitive dependencies. The Java Agent provides the OTel API and SDK.
val bundled = configurations.create("bundled") {
    isCanBeConsumed = false
    isCanBeResolved = true
    isTransitive = false
}

dependencies {
    bundled(project(":flashcat-genai-otel"))

    compileOnly(platform(libs.otel.bom))
    compileOnly(libs.otel.api)
    compileOnly(libs.otel.javaagent.extension.api)
    compileOnly(libs.openai.java)

    testImplementation(platform(libs.otel.bom))
    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly(libs.junit.launcher)
    testImplementation(libs.otel.api)
    testImplementation(libs.otel.sdk)
    testImplementation(libs.otel.sdk.testing)
    testImplementation(libs.openai.java)
    testImplementation(libs.otel.javaagent.extension.api)
}

tasks.test {
    // Run tests serially because they set GlobalOpenTelemetry and system properties.
    maxParallelForks = 1
}

val extensionJar = tasks.register<Jar>("extensionJar") {
    archiveBaseName.set("flashcat-genai-otel-agent-extension")
    archiveVersion.set(project.version.toString())
    destinationDirectory.set(layout.buildDirectory.dir("libs"))
    dependsOn(bundled)
    from({ bundled.resolve().map { zipTree(it) } }) {
        exclude("META-INF/MANIFEST.MF")
    }
    // Include this module's openai-java instrumentation and its InstrumentationModule SPI registration.
    from(sourceSets.main.get().output)
    duplicatesStrategy = DuplicatesStrategy.EXCLUDE
    manifest {
        attributes(
            "Implementation-Title" to "flashcat-genai-otel-agent-extension",
            "Implementation-Version" to project.version,
            "Implementation-Vendor" to "Flashcat",
        )
    }
}

// Verify that the extension jar includes SPI registrations and excludes OpenTelemetry API and SDK classes.
val verifyExtensionJar = tasks.register("verifyExtensionJar") {
    dependsOn(extensionJar)
    val jarFile = extensionJar.flatMap { it.archiveFile }
    inputs.file(jarFile)
    doLast {
        val f = jarFile.get().asFile
        ZipFile(f).use { zip ->
            val names = zip.entries().asSequence().map { it.name }.toList()
            // Check all OpenTelemetry packages, including io/opentelemetry/api and io/opentelemetry/sdk.
            val forbidden = names.filter { it.startsWith("io/opentelemetry/") }
            if (forbidden.isNotEmpty()) {
                throw GradleException("扩展 jar 不能包含 OpenTelemetry 的类：${forbidden.take(10)}")
            }
            val spi = "META-INF/services/io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider"
            if (spi !in names) {
                throw GradleException("扩展 jar 缺少 $spi")
            }
            if ("cloud/flashcat/genai/FlashcatAutoConfigurationCustomizer.class" !in names) {
                throw GradleException("扩展 jar 缺少 SPI 实现类")
            }
            val moduleSpi = "META-INF/services/io.opentelemetry.javaagent.extension.instrumentation.InstrumentationModule"
            if (moduleSpi !in names) {
                throw GradleException("扩展 jar 缺少 $moduleSpi")
            }
            val classes = names.filter { it.endsWith(".class") }
            val outside = classes.filter { !it.startsWith("cloud/flashcat/genai/") }
            if (outside.isNotEmpty()) {
                throw GradleException("扩展 jar 只应包含 cloud.flashcat.genai 的类：${outside.take(10)}")
            }
            logger.lifecycle("verifyExtensionJar: ${f.name} 共 ${classes.size} 个类，均在 cloud/flashcat/genai/ 下，不含 io/opentelemetry/**")
        }
    }
}

// Disable the default jar, which contains only this module's classes and has the same name as extensionJar.
tasks.named<Jar>("jar") { enabled = false }
tasks.named("assemble") { dependsOn(extensionJar) }
tasks.named("check") { dependsOn(verifyExtensionJar) }

publishing {
    publications {
        create<MavenPublication>("maven") {
            artifactId = "flashcat-genai-otel-agent-extension"
            artifact(extensionJar)
            pom {
                name.set("flashcat-genai-otel-agent-extension")
                description.set(project.description)
                url.set("https://github.com/flashcatcloud/flashcat-genai-otel")
                licenses {
                    license {
                        name.set("The Apache License, Version 2.0")
                        url.set("https://www.apache.org/licenses/LICENSE-2.0.txt")
                    }
                }
            }
        }
    }
}
