#!/usr/bin/env python3
"""Фикстура: лиет >16MB на stdout (тест лимита вывода, v0.23)."""
import sys

sys.stdout.write("A" * (17 * 1024 * 1024))
sys.stdout.flush()
