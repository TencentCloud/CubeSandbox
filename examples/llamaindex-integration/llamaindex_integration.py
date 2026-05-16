#!/usr/bin/env python3
"""
LlamaIndex + CubeSandbox Integration Example

This script demonstrates how to use Cube Sandbox as a secure code execution
backend for LlamaIndex agents in RAG workflows.

Requirements:
- Python 3.9+
- pip install llama-index llama-index-agent-openai e2b-code-interpreter python-dotenv

Usage:
1. Set up Cube Sandbox environment (see README_zh.md for setup instructions)
2. Copy .env.example to .env and configure
3. Run: python llamaindex_integration.py
"""

import os
import sys
from typing import Optional

# Load environment variables from .env file if present
try:
    from dotenv import load_dotenv
    load_dotenv()
except ImportError:
    pass


# =============================================================================
# 1. Cube Sandbox Tool for LlamaIndex
# =============================================================================

def create_cube_tool(
    template_id: str,
    api_url: str = "http://127.0.0.1:3000",
    proxy_node_ip: str = "",
    timeout: int = 60,
) -> "FunctionTool":
    """
    Create a LlamaIndex FunctionTool backed by a Cube Sandbox.

    Args:
        template_id: Cube template ID (e.g. tpl-748094d2f2374b0a8a37e6ec)
        api_url: CubeAPI address
        proxy_node_ip: CubeProxy node IP for remote access
        timeout: max execution time in seconds

    Returns:
        LlamaIndex FunctionTool for code execution
    """
    try:
        from llama_index.core.tools import FunctionTool
        from cubesandbox import Sandbox, Config
    except ImportError as e:
        print(f"Error: Missing required package. Install with:")
        print(f"  pip install llama-index cubesandbox")
        sys.exit(1)

    def _run_code(code: str) -> str:
        """Execute Python code in an isolated Cube Sandbox MicroVM."""
        cfg = Config(
            api_url=api_url,
            template_id=template_id,
            proxy_node_ip=proxy_node_ip,
        )
        try:
            with Sandbox.create(config=cfg) as sb:
                result = sb.run_code(code, timeout=timeout)
                if result.error:
                    return f"Error: {result.error.name}: {result.error.value}"
                return result.text or ""
        except Exception as e:
            return f"Execution error: {str(e)}"

    return FunctionTool.from_defaults(
        fn=_run_code,
        name="cube_sandbox",
        description=(
            "Executes Python code inside an isolated Cube Sandbox MicroVM. "
            "Use this for untrusted code, RAG data processing, or any operation "
            "that needs strong isolation. "
            "Input: a single Python code string to execute. "
            "Output: the result of the last expression or stdout/stderr."
        ),
    )


# =============================================================================
# 2. RAG Workflow with Code Execution
# =============================================================================

def create_rag_agent(
    template_id: str,
    api_url: str = "http://127.0.0.1:3000",
    llm_model: str = "gpt-4o",
) -> "ReActAgent":
    """
    Create a LlamaIndex ReAct agent with Cube Sandbox as code execution backend.

    Args:
        template_id: Cube Sandbox template ID
        api_url: CubeAPI address
        llm_model: LLM model to use

    Returns:
        Configured ReActAgent with code execution capabilities
    """
    try:
        from llama_index.core.agent import ReActAgent
        from llama_index.llms.openai import OpenAI
    except ImportError as e:
        print(f"Error: Missing required package. Install with:")
        print(f"  pip install llama-index llama-index-agent-openai")
        sys.exit(1)

    llm = OpenAI(model=llm_model)
    cube_tool = create_cube_tool(template_id=template_id, api_url=api_url)

    agent = ReActAgent.from_tools(
        tools=[cube_tool],
        llm=llm,
        verbose=True,
    )
    return agent


# =============================================================================
# 3. Example: Wikipedia Word Frequency Analysis
# =============================================================================

def example_wikipedia_analysis(agent: "ReActAgent") -> None:
    """
    Example: Download Wikipedia page and compute word frequency statistics.

    This demonstrates a secure RAG workflow where:
    - External data is fetched inside the isolated sandbox
    - Code execution is sandboxed and cannot access host resources
    - Network policies can restrict outbound connections
    """
    query = """
    Download the Wikipedia page about Retrieval-Augmented-Generation (RAG),
    parse it, and compute the top 10 most common words (excluding common stop words).
    """
    print(f"\n{'='*60}")
    print("Example: Wikipedia RAG Word Frequency Analysis")
    print(f"{'='*60}")
    print(f"Query: {query.strip()}\n")

    response = agent.chat(query)
    print(f"\nAgent Response:\n{response}")


# =============================================================================
# 4. Example: Data Processing Pipeline
# =============================================================================

def example_data_processing(agent: "ReActAgent") -> None:
    """
    Example: Process JSON data and compute statistics.
    """
    query = """
    Generate sample sales data for 3 products over 7 days, compute:
    1. Total revenue per product
    2. Daily average revenue
    3. Best performing day for each product
    """
    print(f"\n{'='*60}")
    print("Example: Sales Data Processing")
    print(f"{'='*60}")
    print(f"Query: {query.strip()}\n")

    response = agent.chat(query)
    print(f"\nAgent Response:\n{response}")


# =============================================================================
# 5. Example: Network Isolation for Security
# =============================================================================

def example_secure_execution() -> None:
    """
    Example: Demonstrate network isolation for untrusted code.

    This shows how to configure Cube Sandbox with network policies to:
    - Deny all outbound connections (no data exfiltration)
    - Allow only specific IP ranges
    """
    try:
        from cubesandbox import Sandbox, Config
    except ImportError:
        print("Error: cubesandbox not installed. Run: pip install cubesandbox")
        return

    template_id = os.getenv("CUBE_TEMPLATE_ID", "<your-template-id>")
    api_url = os.getenv("E2B_API_URL", "http://127.0.0.1:3000")

    cfg = Config(api_url=api_url, template_id=template_id)

    print(f"\n{'='*60}")
    print("Example: Secure Execution with Network Isolation")
    print(f"{'='*60}\n")

    # Example 1: No internet access
    print("1. Sandbox with NO internet access:")
    print("-" * 40)
    print("""
    with Sandbox.create(config=cfg, allow_internet_access=False) as sb:
        # Code here cannot access external networks
        # Safe for untrusted data processing
        result = sb.run_code("print('Hello from isolated sandbox!')")
    """)
    print("-" * 40)

    # Example 2: IP whitelist
    print("\n2. Sandbox with IP whitelist (CIDR notation):")
    print("-" * 40)
    print("""
    with Sandbox.create(
        config=cfg,
        allow_internet_access=False,
        network={"allow_out": ["151.101.0.0/16"]}  # GitHub IPs only
    ) as sb:
        result = sb.run_code("import urllib.request; ...")
    """)
    print("-" * 40)


# =============================================================================
# Main: Interactive Demo
# =============================================================================

def main():
    """Main entry point for the LlamaIndex + CubeSandbox demo."""
    print("=" * 60)
    print("LlamaIndex + CubeSandbox Integration Demo")
    print("=" * 60)

    # Load configuration from environment
    template_id = os.getenv("CUBE_TEMPLATE_ID")
    api_url = os.getenv("E2B_API_URL", "http://127.0.0.1:3000")
    openai_key = os.getenv("OPENAI_API_KEY")

    if not template_id:
        print("\n[ERROR] CUBE_TEMPLATE_ID not set!")
        print("\nPlease set up Cube Sandbox and configure environment:")
        print("1. Follow setup instructions in README_zh.md")
        print("2. Create a template: cubemastercli tpl create-from-image ...")
        print("3. Export CUBE_TEMPLATE_ID=<your-template-id>")
        print("\nFor now, showing network isolation examples only...")
        example_secure_execution()
        return

    if not openai_key:
        print("\n[WARN] OPENAI_API_KEY not set. Using mock responses.")
        print("Set OPENAI_API_KEY for full agent functionality.\n")

    print(f"\nConfiguration:")
    print(f"  API URL: {api_url}")
    print(f"  Template ID: {template_id[:20]}...")
    print(f"  LLM: gpt-4o\n")

    # Show network isolation examples
    print("Showing network isolation configuration examples...")
    example_secure_execution()

    if not openai_key:
        print("\n[NOTE] Skipping agent examples - OPENAI_API_KEY not set.")
        print("Set OPENAI_API_KEY to run full agent demonstrations.")
        return

    # Create agent
    try:
        agent = create_rag_agent(template_id=template_id, api_url=api_url)
    except Exception as e:
        print(f"\n[ERROR] Failed to create agent: {e}")
        print("Make sure Cube Sandbox is running at:", api_url)
        return

    # Run examples
    try:
        example_data_processing(agent)
    except Exception as e:
        print(f"\n[ERROR] Data processing example failed: {e}")

    try:
        example_wikipedia_analysis(agent)
    except Exception as e:
        print(f"\n[ERROR] Wikipedia analysis example failed: {e}")


if __name__ == "__main__":
    main()
