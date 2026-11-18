# Migration Summary: @google/generative-ai → @google/genai

## Overview
Successfully migrated `webui/scripts/translate-gemini.ts` from the legacy `@google/generative-ai` (v0.24.1) to the modern `@google/genai` (v3.4.0) SDK.

## Changes Made

### 1. Package Dependencies
**File**: `webui/package.json`
- Removed: `@google/generative-ai: 0.24.1`
- Added: `@google/genai: ^3.4.0`

### 2. Import Changes
**File**: `webui/scripts/translate-gemini.ts` (Line 1-2)
- **Before**: `import { GoogleGenerativeAI, SchemaType } from '@google/generative-ai';`
- **After**: `import { GoogleGenAI } from '@google/genai';`

### 3. Schema Definition Updates
**Lines 71-108**: Replaced `SchemaType` enum with plain JSON schema strings
- `SchemaType.ARRAY` → `'array'`
- `SchemaType.OBJECT` → `'object'`
- `SchemaType.STRING` → `'string'`
- `SchemaType.BOOLEAN` → `'boolean'`

### 4. API Client Initialization
**Lines 237-240**: Updated `GeminiClient` class
```typescript
// Old API
const genAI = new GoogleGenerativeAI(apiKey);
this.model = genAI.getGenerativeModel({ model: MODEL_NAME });

// New API
this.ai = new GoogleGenAI({ apiKey });
```

### 5. API Call Updates
**Lines 243-268**: Updated `generateContent` method call
- Changed from `this.model.generateContent()` to `this.ai.models.generateContent()`
- Updated request structure:
  - Removed nested `contents` with role/parts structure
  - Now accepts `contents` as a simple string
  - `generationConfig` → `config`
  - `responseSchema` → `responseJsonSchema`
  - `result.response.text()` → `result.text` (with null-safety check)

## Key API Differences

| Feature | Old SDK | New SDK |
|---------|---------|---------|
| Client init | `new GoogleGenerativeAI(key)` | `new GoogleGenAI({apiKey: key})` |
| Model selection | `.getGenerativeModel({model})` | Direct call with `model` param |
| Call method | `model.generateContent()` | `ai.models.generateContent()` |
| Config param | `generationConfig` | `config` |
| Schema key | `responseSchema` | `responseJsonSchema` |
| Response text | `result.response.text()` | `result.text` |
| Schema format | Enum-based `SchemaType` | Plain JSON schema strings |

## Verification

**Local Checks (Performed):**
✅ TypeScript compilation: `pnpm run check` - **PASSED**
✅ Project build: `pnpm run build` - **PASSED**
✅ Script parsing: Loads and validates correctly

**Live API Testing:**
The Gemini translation script requires a valid API key and **Node.js 22 or later** (a runtime requirement imposed by @google/genai 3.4.0, not just an API key prerequisite). To verify the migration:
```bash
# Ensure you are using Node.js 22 or later
node --version  # Should show v22.x.x or later

export GEMINI_API_KEY='your-api-key'
cd webui
pnpm run translate
# or for reprocessing mode:
pnpm run translate-reprocess
```

**API Behavior:**
- Request and response schema structure updated to match @google/genai 3.4.0 requirements
- Both `translate` and `translate-reprocess` commands use `responseJsonSchema` with strict validation
- The reprocess result schema uses `oneOf` to define two response types: `{id, ok: true}` (unchanged) or `{id, newTranslation, explanation}` (suggestion). The ok field is constrained to true by enum, and the script validates responses match the schema before filtering. The script filters results by checking for the `newTranslation` field; this selects only suggestion entries. Invalid responses that don't match either schema are rejected with an error.
- Variable placeholders and error handling updated to match new API response format

The commands should work identically to before the migration.

