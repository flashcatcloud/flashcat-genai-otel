import java.time.Duration

// End-to-end tests. This module is excluded from publishing.
//  1. The OpenTelemetry Java Agent, Flashcat extension jar, and a small Java program send data to a mock ingest endpoint.
//  2. A Spring Boot app with OpenTelemetry Spring Boot Starter and flashcat-genai-otel runs with two classpaths.
// The apps run in subprocesses. The tests check data received by the mock endpoint and diagnostics printed by the apps.
plugins {
    java
}

val agentAppSourceSet = sourceSets.create("agentApp")
val springAppSourceSet = sourceSets.create("springApp")

val agentJar = configurations.create("otelAgent") {
    isCanBeConsumed = false
    isTransitive = false
}
// Rerun agentApp with the latest major version of openai-java to check the Agent's instrumentation version coverage.
val agentAppLatestOpenai = configurations.create("agentAppLatestOpenai") {
    isCanBeConsumed = false
}
// Test the semantic API fallback with opentelemetry-api 1.50.0, before 1.57 introduced getOrNoop.
val oldOtelApi = configurations.create("oldOtelApi") {
    isCanBeConsumed = false
    isTransitive = false
}
// Add actuator and micrometer-tracing-bridge-otel to the Spring Boot app's classpath.
// Resolve both together with springApp's runtime dependencies as one classpath.
val springAppWithActuator = configurations.create("springAppWithActuator") {
    isCanBeConsumed = false
    extendsFrom(configurations["springAppRuntimeClasspath"])
}

dependencies {
    agentJar(libs.otel.javaagent)

    "agentAppImplementation"(project(":flashcat-genai-otel"))
    "agentAppImplementation"(libs.openai.java)

    agentAppLatestOpenai(project(":flashcat-genai-otel"))
    agentAppLatestOpenai(libs.openai.java.latest)

    oldOtelApi(project(":flashcat-genai-otel"))
    oldOtelApi("io.opentelemetry:opentelemetry-api:1.50.0")
    oldOtelApi("io.opentelemetry:opentelemetry-context:1.50.0")

    "springAppImplementation"(platform(libs.spring.boot.bom))
    "springAppImplementation"(platform(libs.otel.instrumentation.bom))
    "springAppImplementation"(libs.spring.boot.starter)
    "springAppImplementation"(libs.otel.spring.boot.starter)
    "springAppImplementation"(project(":flashcat-genai-otel"))

    springAppWithActuator(platform(libs.spring.boot.bom))
    springAppWithActuator(platform(libs.otel.instrumentation.bom))
    springAppWithActuator("org.springframework.boot:spring-boot-starter-actuator")
    springAppWithActuator("io.micrometer:micrometer-tracing-bridge-otel")

    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly(libs.junit.launcher)
    testImplementation(testFixtures(project(":flashcat-genai-otel")))
}

tasks.named<JavaCompile>("compileAgentAppJava") { options.release.set(8) }
tasks.named<JavaCompile>("compileSpringAppJava") { options.release.set(17) }
tasks.named<JavaCompile>("compileTestJava") { options.release.set(17) }

// :flashcat-genai-otel-agent-extension:extensionJar builds the extension jar.
val extensionJarTask = project(":flashcat-genai-otel-agent-extension").tasks.named<Jar>("extensionJar")

tasks.test {
    dependsOn(extensionJarTask, agentAppSourceSet.output, springAppSourceSet.output)
    inputs.files(agentJar, oldOtelApi, extensionJarTask, agentAppSourceSet.runtimeClasspath, springAppSourceSet.runtimeClasspath)
    val javaBin = javaLauncher.map { it.executablePath.asFile.absolutePath }
    doFirst {
        systemProperty("it.java", javaBin.get())
        systemProperty("it.agentJar", agentJar.singleFile.absolutePath)
        systemProperty("it.extensionJar", extensionJarTask.get().archiveFile.get().asFile.absolutePath)
        systemProperty("it.agentApp.classpath", agentAppSourceSet.runtimeClasspath.asPath)
        systemProperty(
            "it.agentAppLatestOpenai.classpath",
            (agentAppSourceSet.output + agentAppLatestOpenai).asPath,
        )
        systemProperty("it.oldApi.classpath", (agentAppSourceSet.output + oldOtelApi).asPath)
        systemProperty("it.springApp.classpath", springAppSourceSet.runtimeClasspath.asPath)
        systemProperty(
            "it.springAppActuator.classpath",
            (springAppSourceSet.output + springAppWithActuator).asPath,
        )
    }
    // Subprocesses can take tens of seconds.
    timeout.set(Duration.ofMinutes(10))
}
