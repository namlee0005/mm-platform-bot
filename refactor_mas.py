import os
import shutil
import glob

# 1. Clean up garbage files
for trash in ['internal/strategy/mas_test_ping.go', 'internal/strategy/mas_test_final.go', 'internal/strategy/mas_strategy_test.go']:
    try:
        os.remove(trash)
    except OSError:
        pass

# 2. Create the package directory
os.makedirs('internal/strategy/mas', exist_ok=True)

# 3. Move files
mas_files = glob.glob('internal/strategy/mas_*.go')
for f in mas_files:
    shutil.move(f, f.replace('internal/strategy/', 'internal/strategy/mas/'))

# 4. Update package declarations
for f in glob.glob('internal/strategy/mas/*.go'):
    with open(f, 'r') as file:
        content = file.read()
    
    content = content.replace('package strategy', 'package mas')
    
    with open(f, 'w') as file:
        file.write(content)

