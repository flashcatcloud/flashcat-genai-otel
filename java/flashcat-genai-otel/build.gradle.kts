plugins {
    `java-library`
    `java-test-fixtures`
    `maven-publish`
}

description = "Flashcat GenAI OTel SDK for Java: AI Agent observability on top of OpenTelemetry"

java {
    withSourcesJar()
    withJavadocJar()
}

tasks.named<JavaCompile>("compileJava") {
    // Target Java 8 bytecode for published artifacts.
    options.release.set(8)
    options.compilerArgs.addAll(listOf("-Xlint:all,-options,-serial,-processing", "-Werror"))
}
tasks.named<JavaCompile>("compileTestFixturesJava") { options.release.set(8) }
tasks.named<JavaCompile>("compileTestJava") { options.release.set(8) }

dependencies {
    api(platform(libs.otel.bom))
    api(libs.otel.api)
    // Programmatic initialization and Spring Boot Starter require the SDK. In Java agent mode, the agent supplies these classes and the application copy goes unused.
    implementation(libs.otel.sdk)
    implementation(libs.otel.sdk.autoconfigure)
    implementation(libs.otel.exporter.otlp)

    testFixturesApi(platform(libs.otel.bom))
    testFixturesApi(libs.otel.proto)
    testFixturesApi(libs.protobuf.java)

    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly(libs.junit.launcher)
    testImplementation(libs.otel.sdk.testing)
    testImplementation(libs.otel.sdk)
    testImplementation(libs.otel.sdk.autoconfigure)
}

tasks.test {
    // Run test classes sequentially because each resets GlobalOpenTelemetry and the SDK's static state.
    maxParallelForks = 1
    systemProperty("flashcat.test.fixtures.dir", rootProject.projectDir.parentFile.resolve("spec/fixtures").absolutePath)
    systemProperty("flashcat.test.expected.version", project.version.toString())
}

tasks.jar {
    manifest {
        attributes(
            "Implementation-Title" to "flashcat-genai-otel",
            "Implementation-Version" to project.version,
            "Implementation-Vendor" to "Flashcat",
        )
    }
}

// Test fixtures are for repository tests only and are excluded from published artifacts.
val javaComponent = components["java"] as AdhocComponentWithVariants
javaComponent.withVariantsFromConfiguration(configurations["testFixturesApiElements"]) { skip() }
javaComponent.withVariantsFromConfiguration(configurations["testFixturesRuntimeElements"]) { skip() }

publishing {
    publications {
        create<MavenPublication>("maven") {
            artifactId = "flashcat-genai-otel"
            from(components["java"])
            pom {
                name.set("flashcat-genai-otel")
                description.set(project.description)
                url.set("https://github.com/flashcatcloud/flashcat-genai-otel")
                licenses {
                    license {
                        name.set("The Apache License, Version 2.0")
                        url.set("https://www.apache.org/licenses/LICENSE-2.0.txt")
                    }
                }
                scm {
                    url.set("https://github.com/flashcatcloud/flashcat-genai-otel")
                }
            }
        }
    }
    // Publishing is handled outside this repository, so no remote repositories are configured.
}
