#!/usr/bin/env python3
"""Фикстура: краш с exit 2 → платформенная ошибка, стоп всего рана."""
import sys

print("boom: segmentation fault (притворный)", file=sys.stderr)
sys.exit(2)
