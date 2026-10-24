#!/usr/bin/env python3
"""Run the same standalone helper that Broker serves to client devices."""
from pathlib import Path
import runpy

runpy.run_path(str(Path(__file__).resolve().parents[1] / "internal/webassets/static/usage-sync.py"), run_name="__main__")
