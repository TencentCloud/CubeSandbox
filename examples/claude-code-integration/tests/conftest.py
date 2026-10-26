"""Pytest configuration — add parent directory to sys.path for imports."""
import sys
import os

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
