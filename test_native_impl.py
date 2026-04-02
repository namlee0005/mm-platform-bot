import re

main_path = 'cmd/main.go'
with open(main_path, 'r') as f:
    content = f.read()

# Swap NewCCXTExchange to NewNativeExchange if implemented
# First we need to write the native adapter itself

