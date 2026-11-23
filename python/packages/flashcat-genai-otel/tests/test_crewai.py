"""Test catalog loading for OpenInference CrewAI instrumentation with a minimal mock distribution to avoid heavy CrewAI dependencies such as chromadb and lancedb."""

from __future__ import annotations

import textwrap

FAKE = {
    "crewai/__init__.py": """
        class Task:
            def _execute_core(self, *a, **k): return "task"
        class Crew:
            def kickoff(self, *a, **k): return "crew"
        class Agent:
            def kickoff(self, *a, **k): return "agent"
        from crewai.flow.flow import Flow
    """,
    "crewai/agent/__init__.py": "",
    "crewai/agent/core.py": """
        class Agent:
            def _execute_without_timeout(self, *a, **k): return "agent"
    """,
    "crewai/flow/__init__.py": "",
    "crewai/flow/flow.py": """
        class Flow:
            def kickoff(self, *a, **k): return "flow"
            async def kickoff_async(self, *a, **k): return "flow"
            def _execute_method(self, *a, **k): return "method"
    """,
    "crewai/tools/__init__.py": "",
    "crewai/tools/base_tool.py": """
        class BaseTool:
            def run(self, *a, **k): return "tool"
    """,
    "crewai-1.15.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: crewai\nVersion: 1.15.0\n",
    "crewai-1.15.0.dist-info/RECORD": "",
}


def fake_crewai(tmp_path) -> str:
    for rel, body in FAKE.items():
        p = tmp_path / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(textwrap.dedent(body))
    return str(tmp_path)


SCRIPT = """
import json
from flashcat import genai
diag = genai.init(service_name="x", metrics=False, exporter="none")
print("RESULT:" + json.dumps(next(i for i in diag["instrumentations"] if i["lib"] == "crewai")))
"""


def test_crewai_loads_openinference_instrumentation(tmp_path, run_script):
    item = run_script(SCRIPT, {"PYTHONPATH": fake_crewai(tmp_path)})
    assert item == {"lib": "crewai", "source": "openinference", "lib_version": "1.15.0", "status": "installed"}


def test_crewai_without_openinference_package_gives_install_hint(tmp_path, run_script):
    # Hide the OpenInference package to simulate installing crewai without flashcat-genai-otel[crewai].
    blocker = tmp_path / "blocker"
    (blocker / "openinference" / "instrumentation" / "crewai").mkdir(parents=True)
    (blocker / "openinference" / "instrumentation" / "crewai" / "__init__.py").write_text("raise ImportError('blocked')\n")
    (blocker / "openinference" / "__init__.py").write_text("")
    (blocker / "openinference" / "instrumentation" / "__init__.py").write_text("")
    path = f"{blocker}:{fake_crewai(tmp_path / 'site')}"
    item = run_script(SCRIPT, {"PYTHONPATH": path})
    assert item["status"] == "instrumentation_not_installed"
    assert item["hint"] == "pip install 'flashcat-genai-otel[crewai]'"
