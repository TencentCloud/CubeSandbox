from __future__ import annotations

import itertools
import os
import sys

from dotenv import load_dotenv
from langchain.agents import create_agent
from langchain_core.tools import tool
from langchain_openai import ChatOpenAI
from cubesandbox import Sandbox

load_dotenv()

for v in ("CUBE_TEMPLATE_ID",):
    if not os.environ.get(v):
        print(f"Missing env: {v}", file=sys.stderr)
        sys.exit(1)

_llm_key = os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY")
if not _llm_key:
    print("Missing LLM API key (set OPENAI_API_KEY or TOKENHUB_API_KEY)", file=sys.stderr)
    sys.exit(1)

# If the CubeAPI endpoint uses a self-signed CA, export it so both the
# control-plane client (requests → REQUESTS_CA_BUNDLE) and the data-plane
# client (httpx/envd RPCs → SSL_CERT_FILE) trust it. The process-global
# export also applies to the LLM client; the bundle must include public
# root CAs (or set CUBE_SSL_CERT_FILE only when the LLM uses the same CA).
_cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE")
if _cube_ssl and os.path.isfile(_cube_ssl):
    os.environ["SSL_CERT_FILE"] = _cube_ssl
    os.environ["REQUESTS_CA_BUNDLE"] = _cube_ssl


SANDBOX_CONTEXT = (
    "You are a data analyst. You can execute Python inside a Cube Sandbox "
    "MicroVM via the run_python tool. Environment facts:\n"
    "- Working directory: /workspace\n"
    "- Demo dataset: /workspace/sales.csv with columns month,product,units,price\n"
    "  (6 rows: 3 months x 2 products; revenue is defined as units * price)\n"
    "- Preinstalled: pandas, numpy, matplotlib, scikit-learn\n"
    "- Save any charts/artifacts under /workspace\n"
    "When the user mentions 'the dataset' without a path, use /workspace/sales.csv.\n"
    "Modeling conventions for this tiny demo dataset (follow them unless the "
    "user explicitly specifies otherwise):\n"
    "- Regression/forecast target: monthly TOTAL revenue. Aggregate to one row "
    "per month, then use a numeric month index (0, 1, 2, ...) as the only "
    "feature.\n"
    "- Never use the target itself or its direct components (units, price) as "
    "features when predicting revenue - that is data leakage and yields a "
    "meaningless RMSE of 0.\n"
    "- The dataset is too small for a train/test split; fit and evaluate on "
    "all rows and explicitly state that the metric is in-sample.\n"
    "- Report only numbers actually printed by the executed code; never "
    "invent or estimate metric values."
)


def build_agent(llm, sandbox: Sandbox):
    """Build the LangChain 1.x agent with a run_python tool bound to `sandbox`."""

    _script_counter = itertools.count()

    @tool
    def run_python(code: str) -> str:
        """Execute Python inside the Cube Sandbox MicroVM; return stdout, with stderr
        delimited below it when present.

        The MicroVM is preinstalled with pandas / numpy / matplotlib / scikit-learn.
        Each call writes the snippet to a unique /workspace/_agent_<n>.py and runs it,
        so concurrent tool calls don't overwrite each other.
        Charts can be saved under /workspace (e.g. /workspace/revenue.png).
        """
        script = f"/workspace/_agent_{next(_script_counter)}.py"
        sandbox.files.write(script, code)
        result = sandbox.commands.run(f"python3 {script}", timeout=120, cwd="/workspace")
        out = result.stdout
        # Keep stderr delimited from stdout so library warnings (exit_code 0)
        # don't blur the real output seen by the LLM.
        if result.stderr:
            out += "\n--- stderr ---\n" + result.stderr
        if result.exit_code != 0:
            out += f"\n[non-zero exit code: {result.exit_code}]"
        return out

    return create_agent(llm, [run_python], system_prompt=SANDBOX_CONTEXT)


if __name__ == "__main__":
    llm = ChatOpenAI(
        model=os.getenv("CHAT_MODEL") or "deepseek-v3",
        api_key=_llm_key,
        base_url=os.getenv("OPENAI_BASE_URL", "https://tokenhub.tencentmaas.com/v1"),
        timeout=60, max_retries=2, temperature=0,
    )

    question = sys.argv[1] if len(sys.argv) > 1 else (
        "Load sales.csv from /workspace, compute total revenue per month, "
        "and report the month -> revenue numbers in your final answer."
    )

    # One MicroVM for the whole run; reused across every run_python call.
    # The context manager tears the sandbox down on exit, so nothing is left behind.
    with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
        print(f"Sandbox {sandbox.sandbox_id} created. Running agent...")
        agent = build_agent(llm, sandbox)
        result = agent.invoke({"messages": [{"role": "user", "content": question}]})
        # The last message may have empty .content (truncated run, or final turn
        # is a tool call). Scan backwards for the last non-empty answer.
        for msg in reversed(result["messages"]):
            if msg.content:
                print(msg.content)
                break
        else:
            print("(no final answer in messages)")
