// Validation of untrusted host JSON against the generated contract schema (sdk.md S2).
// Hand-written; the schema it walks is generated into ./contract.ts.

import { CONTRACT_SCHEMA } from './contract.ts';
import type { ContractMessageName, ContractMessages, FieldSchema } from './contract.ts';

export type ValidationResult<T> = { ok: true; value: T } | { ok: false; errors: string[] };

const BASE64 = /^[A-Za-z0-9+/_-]*={0,2}$/;
const DECIMAL = /^-?[0-9]+$/;
const INT64_MINIMUM = -(2n ** 63n);
const INT64_MAXIMUM = 2n ** 63n - 1n;
const UINT64_MAXIMUM = 2n ** 64n - 1n;

function isPlainObject(value: unknown): value is Record<string, unknown> {
	return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function defaultValue(field: FieldSchema): unknown {
	switch (field.kind) {
		case 'bool':
			return false;
		case 'string':
		case 'bytes':
			return '';
		case 'int32':
		case 'uint32':
		case 'double':
			return 0;
		case 'int64':
		case 'uint64':
			return '0';
		case 'enum':
			return CONTRACT_SCHEMA.enums[field.type ?? '']?.[0];
		case 'message':
			return undefined;
	}
}

function checkInteger64(field: FieldSchema, value: unknown, path: string, errors: string[]): unknown {
	let text: string;
	if (typeof value === 'number' && Number.isSafeInteger(value)) {
		text = String(value);
	} else if (typeof value === 'string' && DECIMAL.test(value)) {
		text = value;
	} else {
		errors.push(`${path}: want a 64-bit integer as a decimal string`);
		return undefined;
	}
	const parsed = BigInt(text);
	const minimum = field.kind === 'uint64' ? 0n : INT64_MINIMUM;
	const maximum = field.kind === 'uint64' ? UINT64_MAXIMUM : INT64_MAXIMUM;
	if (parsed < minimum || parsed > maximum) {
		errors.push(`${path}: ${text} is out of range for ${field.kind}`);
		return undefined;
	}
	return parsed.toString();
}

function checkValue(field: FieldSchema, value: unknown, path: string, errors: string[]): unknown {
	switch (field.kind) {
		case 'bool':
			if (typeof value === 'boolean') return value;
			errors.push(`${path}: want a boolean`);
			return undefined;
		case 'string':
			if (typeof value === 'string') return value;
			errors.push(`${path}: want a string`);
			return undefined;
		case 'bytes':
			if (typeof value === 'string' && BASE64.test(value)) return value;
			errors.push(`${path}: want base64 bytes`);
			return undefined;
		case 'int32':
			if (typeof value === 'number' && Number.isInteger(value) && value >= -(2 ** 31) && value < 2 ** 31) return value;
			errors.push(`${path}: want a 32-bit integer`);
			return undefined;
		case 'uint32':
			if (typeof value === 'number' && Number.isInteger(value) && value >= 0 && value < 2 ** 32) return value;
			errors.push(`${path}: want an unsigned 32-bit integer`);
			return undefined;
		case 'int64':
		case 'uint64':
			return checkInteger64(field, value, path, errors);
		case 'double':
			if (typeof value === 'number' || value === 'NaN' || value === 'Infinity' || value === '-Infinity') return value;
			errors.push(`${path}: want a number`);
			return undefined;
		case 'enum': {
			const names = CONTRACT_SCHEMA.enums[field.type ?? ''] ?? [];
			if (typeof value === 'string' && names.includes(value)) return value;
			errors.push(`${path}: ${JSON.stringify(value)} is not a ${field.type} value`);
			return undefined;
		}
		case 'message':
			return checkMessage(field.type as ContractMessageName, value, path, errors);
	}
}

function checkMessage(name: ContractMessageName, value: unknown, path: string, errors: string[]): Record<string, unknown> | undefined {
	const fields = CONTRACT_SCHEMA.messages[name];
	if (fields === undefined) {
		errors.push(`${path}: unknown message ${name}`);
		return undefined;
	}
	if (!isPlainObject(value)) {
		errors.push(`${path}: want a ${name} object`);
		return undefined;
	}
	const result: Record<string, unknown> = {};
	for (const field of fields) {
		const fieldPath = `${path}.${field.json}`;
		const raw = value[field.json];
		if (raw === undefined || raw === null) {
			if (field.shape === 'list') result[field.json] = [];
			else if (field.shape === 'map') result[field.json] = {};
			else if (field.shape === 'single' && field.kind !== 'message') result[field.json] = defaultValue(field);
			continue;
		}
		if (field.shape === 'list') {
			if (!Array.isArray(raw)) {
				errors.push(`${fieldPath}: want a list`);
				continue;
			}
			result[field.json] = raw.map((item, index) => checkValue(field, item, `${fieldPath}[${index}]`, errors));
		} else if (field.shape === 'map') {
			if (!isPlainObject(raw)) {
				errors.push(`${fieldPath}: want a map`);
				continue;
			}
			const map: Record<string, unknown> = {};
			for (const [key, item] of Object.entries(raw)) {
				map[key] = checkValue(field, item, `${fieldPath}[${JSON.stringify(key)}]`, errors);
			}
			result[field.json] = map;
		} else {
			result[field.json] = checkValue(field, raw, fieldPath, errors);
		}
	}
	return result;
}

/**
 * Checks untrusted host JSON against message `name` of the contract. On success the value
 * has every list, map and plain scalar field present, with protojson defaults filled in for
 * omitted ones; an absent message field or proto3 `optional` field stays absent. Properties
 * the contract does not define are dropped, so a newer producer stays compatible.
 */
export function validate<K extends ContractMessageName>(name: K, value: unknown): ValidationResult<ContractMessages[K]> {
	const errors: string[] = [];
	const result = checkMessage(name, value, name, errors);
	if (errors.length > 0 || result === undefined) {
		return { ok: false, errors };
	}
	return { ok: true, value: result as ContractMessages[K] };
}
