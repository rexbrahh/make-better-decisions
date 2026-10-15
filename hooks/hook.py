"""Run the Go engine for a host session that loaded a hooks.json from before version 2.0.0."""
import os
import sys

binary = os.path.join(os.path.dirname(os.path.dirname(os.path.realpath(__file__))), "bin", "agent-decisions")
os.execv(binary, [binary, "hook"] + sys.argv[1:])
