plugins {
    base
}

allprojects {
    group = "cloud.flashcat"
    version = "0.2.0"
}

subprojects {
    tasks.withType<JavaCompile>().configureEach {
        options.encoding = "UTF-8"
    }
    tasks.withType<Javadoc>().configureEach {
        options.encoding = "UTF-8"
        (options as StandardJavadocDocletOptions).apply {
            charSet = "UTF-8"
            docEncoding = "UTF-8"
            addStringOption("Xdoclint:none", "-quiet")
        }
    }
    tasks.withType<Test>().configureEach {
        useJUnitPlatform()
        // Keep OTEL_* and FLASHCAT_* settings from the developer's machine out of test processes.
        environment.keys.filter { it.startsWith("OTEL_") || it.startsWith("FLASHCAT_") }.forEach { environment.remove(it) }
        systemProperty("file.encoding", "UTF-8")
        testLogging {
            events("failed")
            exceptionFormat = org.gradle.api.tasks.testing.logging.TestExceptionFormat.FULL
            showStandardStreams = false
        }
    }
}
