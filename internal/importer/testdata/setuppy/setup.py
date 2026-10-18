from setuptools import setup

# Исполняемый код чужого автора: импортёр его не запускает и не разбирает.
setup(name="legacy-tool", version="1.0.0", install_requires=["requests"])
