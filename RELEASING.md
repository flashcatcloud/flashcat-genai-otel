# Releasing and installing

GitHub Releases contain installable artifacts, and Go packages use module tags. The Python packages also have a dedicated publishing workflow. npm and Maven Central releases are pending.

## Releasing

1. Set the same version for all four languages in `python/packages/flashcat-genai-otel/pyproject.toml`, `python/instrumentation/flashcat-instrumentation-dashscope/pyproject.toml`, `python/instrumentation/flashcat-instrumentation-mcp/pyproject.toml`, `js/package.json`, and `java/build.gradle.kts`. Go versions are set by tags alone.
2. Run the tests for all four languages using the commands in the README. You can try a local build first with `scripts/build-release.sh <version>`.
3. Run `scripts/tag-release.sh <version>` on the commit you want to release. This creates and pushes `v<version>` and the tags for the four Go modules (`go/v<version>`, `go/instrumentation/openai/v<version>`, `go/instrumentation/eino/v<version>`, `go/instrumentation/mcp/v<version>`).
4. The `v<version>` tag triggers `.github/workflows/release.yml`, which runs `scripts/build-release.sh` and creates a release with the files below.

| File | Purpose |
|---|---|
| `flashcat_genai_otel-<version>-py3-none-any.whl`, `flashcat_instrumentation_dashscope-<version>-py3-none-any.whl`, `flashcat_instrumentation_mcp-<version>-py3-none-any.whl` (and sdists) | Python |
| `flashcat-genai-otel-<version>.tgz` | Node.js (`@flashcat/genai-otel`) |
| `flashcat-genai-otel-agent-extension-<version>.jar` | Java Agent extension |
| `flashcat-genai-otel-<version>.jar` / `.pom` | Java application API (`cloud.flashcat:flashcat-genai-otel`) |
| `flashcat-genai-otel-java-maven-repo-<version>.zip` | File-based Maven repository containing the two Java artifacts above. Use it as a local repository or import it into Nexus or Artifactory |
| `SHA256SUMS` | Checksums for all files |

## Publish Python packages

Configure GitHub Trusted Publishers on each package index. Use owner `flashcatcloud`, repository `flashcat-genai-otel`, and workflow `python-publish.yml`. Each package uses a separate environment so its pending publisher can be registered before the first release.

| Package | TestPyPI environment | PyPI environment |
|---|---|---|
| `flashcat-instrumentation-dashscope` | `testpypi` | `pypi` |
| `flashcat-instrumentation-mcp` | `testpypi-mcp` | `pypi-mcp` |
| `flashcat-genai-otel` | `testpypi-sdk` | `pypi-sdk` |

Run the `Publish Python packages` workflow on `main` with `testpypi` selected. It publishes the two instrumentation packages before the SDK and checks the `[all]` install in a fresh environment. After the test run passes, run the workflow from the same commit with `pypi` selected and check the production install job.

## Install from GitHub Releases

The examples below use `v0.1.0`, with the download URL prefix `https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0`.

**Python** (`[all]` includes DashScope and MCP instrumentation from the release, with the remaining dependencies installed from PyPI)

```bash
mkdir -p flashcat-genai-otel-pkgs
for f in flashcat_genai_otel flashcat_instrumentation_dashscope flashcat_instrumentation_mcp; do
  curl -fL -o flashcat-genai-otel-pkgs/$f-0.1.0-py3-none-any.whl https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0/$f-0.1.0-py3-none-any.whl
done
pip install --find-links flashcat-genai-otel-pkgs 'flashcat-genai-otel[all]'
```

If you only use instrumentation available outside the release, such as `[openai]`, you can install directly from the wheel URL with `pip install 'flashcat-genai-otel[openai] @ https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0/flashcat_genai_otel-0.1.0-py3-none-any.whl'`.

**Node.js**

```bash
npm install https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0/flashcat-genai-otel-0.1.0.tgz
```

**Go**

```bash
go get github.com/flashcatcloud/flashcat-genai-otel/go@v0.1.0
go get github.com/flashcatcloud/flashcat-genai-otel/go/instrumentation/openai@v0.1.0   # If you use openai-go
go get github.com/flashcatcloud/flashcat-genai-otel/go/instrumentation/eino@v0.1.0     # If you use Eino
go get github.com/flashcatcloud/flashcat-genai-otel/go/instrumentation/mcp@v0.1.0      # If you use the MCP Go SDK
```

**Java** (use the extension with upstream OpenTelemetry Java Agent 2.33.1)

```bash
mkdir -p /opt/flashcat
curl -fL -o /opt/flashcat/opentelemetry-javaagent.jar \
  https://github.com/open-telemetry/opentelemetry-java-instrumentation/releases/download/v2.33.1/opentelemetry-javaagent.jar
curl -fL -o /opt/flashcat/flashcat-genai-otel-agent-extension-0.1.0.jar https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0/flashcat-genai-otel-agent-extension-0.1.0.jar
```

To use `cloud.flashcat:flashcat-genai-otel` to annotate your code with calls such as `Flashcat.agent` / `tool` / `session`, extract the Maven repository ZIP and add it as a repository, or install the artifacts in your local repository as shown below.

```bash
curl -fL -O https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0/flashcat-genai-otel-0.1.0.jar
curl -fL -O https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0/flashcat-genai-otel-0.1.0.pom
mvn install:install-file -Dfile=flashcat-genai-otel-0.1.0.jar -DpomFile=flashcat-genai-otel-0.1.0.pom
```

For on-premises environments, copy these files to the internal network (private PyPI, npm, or Maven repositories, GOPROXY, or vendor) and install from there.
