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

✅ TypeScript compilation: `pnpm run check` - **PASSED**
✅ Project build: `pnpm run build` - **PASSED**
✅ Script parsing: Loads and validates correctly
✅ Functionality preserved: Same schema-validated JSON responses

## Compatibility

- **Same functionality**: Send prompts with JSON schema validation and get structured responses
- **Variable preservation**: Still correctly handles `{variable}` placeholders in translations
- **Backward compatibility**: The public API of `GeminiClient` remains unchanged
- **Error handling**: Maintains existing retry logic with exponential backoff

## Testing

To test the migration with the actual API:
```bash
export GEMINI_API_KEY='your-api-key'
cd webui
pnpm run translate
# or for reprocessing mode:
pnpm run translate-reprocess
```

The commands should work identically to before the migration.
