#!/usr/bin/env python3
"""Фикстура: рожает дочерний sleep и выходит (тест process-group kill, v0.23).
pid дочернего — в файл $SPID_FILE."""
import os
import subprocess
import sys

# v0.29: sys.executable вместо "sleep" — фикстура переносима (Windows/Unix).
child = subprocess.Popen(
    [sys.executable, "-c", "import time; time.sleep(30)"])
with open(os.environ["SPID_FILE"], "w") as f:
    f.write(str(child.pid))
