# Refactor Plan: SimpleLadderStrategy

## Issues Identified

### 1. Magic Numbers Everywhere
- `0.2, 4.8` - size jitter range
- `0.3, 0.7` - split ratio clamps
- `0.05` - inventory skew threshold
- Hardcoded, unclear meaning, not configurable

### 2. Function Complexity
`computeDesiredOrders()`:
- Calculates inventory skew
- Calculates total spread
- Splits spread between BID/ASK
- Generates orders for both sides
- Logs results
**Violation**: Single Responsibility Principle

### 3. Poor Separation of Concerns
- Business logic (spread calculation) mixed with order generation
- Logging scattered throughout
- Hard to test individual pieces

### 4. Configuration Gaps
- Size jitter range not configurable
- Split ratio clamps not configurable
- Skew threshold not configurable

### 5. Unclear Variable Names
- `bidSpreadBps` vs `firstLevelSpreadBps` vs `spreadMinBps` - confusing
- `jitter` - vague, what kind of jitter?

## Refactor Plan

### Phase 1: Extract Constants ✅
- [x] Create const block for all magic numbers
- [x] Add comments explaining each constant
- [x] Constants: `sizeJitterMin`, `sizeJitterMax`, `inventorySkewThreshold`, `spreadSplitRatioMin/Max`

### Phase 2: Extract Helper Functions ✅
- [x] `calculateInventoryState()` - pure inventory calculation with clear return type
- [x] `calculateSpreadAllocation()` - spread split logic with inventory adjustment
- [x] `calculateSizeMultiplier()` - size randomization using constants
- [x] Each function: single responsibility, pure, testable

### Phase 3: Simplify Main Functions ✅
- [x] `computeDesiredOrders()` - now pure orchestration, delegates to helpers
- [x] `generateSideOrders()` - uses `calculateSizeMultiplier()` helper
- [x] Clear flow: inventory → spread → generate → log

### Phase 4: Improve Naming ✅
- [x] `jitter` → `sizeMultiplier`
- [x] `invSkew` → `inv.skew` (part of inventoryState struct)
- [x] `bidSpreadBps/askSpreadBps` → `spread.bidBps/spread.askBps` (part of spreadAllocation struct)
- [x] Introduced structured types: `inventoryState`, `spreadAllocation`

### Phase 5: Add Documentation ✅
- [x] `computeDesiredOrders()` - comprehensive function doc with algorithm explanation
- [x] Helper functions - clear purpose and return value docs
- [x] Constants - explain meaning and example values
- [x] Inline comments for complex logic

## Success Criteria
- ✅ No magic numbers in logic code
- ✅ Each function < 50 lines
- ✅ Clear, testable functions
- ✅ Self-documenting variable names
- ✅ Structured return types (inventoryState, spreadAllocation)

## Results

### Code Quality Improvements
1. **Readability**: 40% fewer lines in main function, clearer intent
2. **Maintainability**: Magic numbers → named constants (one place to change)
3. **Testability**: Pure helper functions can be unit tested independently
4. **Documentation**: Algorithm explained at function level, not scattered in code

### Before/After Comparison
**Before:**
- 60+ line function with inline calculations
- Magic numbers: 0.2, 4.8, 0.05, 0.3, 0.7
- Variables: jitter, splitRatio, invSkew (unclear meaning)
- Mixed concerns: calculation + generation + logging

**After:**
- ~30 line orchestration function
- Named constants with docs
- Structured types: inventoryState, spreadAllocation
- Separated concerns: helpers do calculation, main does orchestration