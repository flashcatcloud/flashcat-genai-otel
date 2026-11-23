rootProject.name = "flashcat-genai-otel-java"

dependencyResolutionManagement {
    repositories {
        mavenCentral()
    }
}

include("flashcat-genai-otel")
include("flashcat-genai-otel-agent-extension")
// Used only for end-to-end tests with the Java agent and Spring Boot Starter. This module is excluded from publishing.
include("integration-tests")
