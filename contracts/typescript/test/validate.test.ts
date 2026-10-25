import { test } from 'node:test';
import assert from 'node:assert/strict';

import { CONTRACT_SCHEMA, validate } from '../src/index.ts';

const descriptor = {
	token: 'clc',
	contractVersion: '1.2.0',
	nav: [{ label: 'Overview', path: '/' }, { label: 'Workloads', path: '/workloads', group: 'Runtime' }],
	descriptorSchemaMajor: 1,
	ui: {
		kind: 'UI_KIND_SHELL',
		elementName: 'clc-app',
		contractMajor: 1,
		slots: [{ id: 'jobs', schemaMajor: 1, title: 'clc.jobs', providerPath: '/m/clc/api/jobs' }],
	},
	neededMycelLicense: ['clc'],
	routeClasses: [{ method: 'POST', path: '/m/clc/api/ingest', class: 'ROUTE_CLASS_NETWORK_SERVICE' }],
	openapi: 'eyJvcGVuYXBpIjoiMy4wLjMifQ==',
	somethingNewer: { ignored: true },
};

test('a valid descriptor passes with defaults filled in and unknown properties dropped', () => {
	const result = validate('DescribeResponse', descriptor);
	assert.ok(result.ok, JSON.stringify(result));
	assert.equal(result.value.menuGroup, '');
	assert.deepEqual(result.value.capabilities, []);
	assert.equal(result.value.ui?.entryAsset, '');
	assert.deepEqual(result.value.ui?.browserRoutes, []);
	assert.equal(result.value.ui?.minContractMinor, 0);
	assert.equal(result.value.nav?.[0]?.group, undefined, 'an absent optional field stays absent');
	assert.equal(result.value.nav?.[1]?.group, 'Runtime');
	assert.equal('somethingNewer' in result.value, false);
});

test('wrong shapes are refused with a path', () => {
	const cases: Array<[string, unknown, string]> = [
		['not an object', [], 'DescribeResponse'],
		['unknown enum', { ui: { kind: 'UI_KIND_FLOATING' } }, 'DescribeResponse.ui.kind'],
		['enum as number', { ui: { kind: 1 } }, 'DescribeResponse.ui.kind'],
		['negative uint32', { descriptorSchemaMajor: -1 }, 'DescribeResponse.descriptorSchemaMajor'],
		['fractional uint32', { descriptorSchemaMajor: 1.5 }, 'DescribeResponse.descriptorSchemaMajor'],
		['uint32 as string', { descriptorSchemaMajor: '1' }, 'DescribeResponse.descriptorSchemaMajor'],
		['string list item', { capabilities: ['jobs', 7] }, 'DescribeResponse.capabilities[1]'],
		['list not array', { nav: { label: 'x' } }, 'DescribeResponse.nav'],
		['bad base64', { openapi: '{not base64}' }, 'DescribeResponse.openapi'],
		['nested message', { routeClasses: [{ class: 'ROUTE_CLASS_X' }] }, 'DescribeResponse.routeClasses[0].class'],
	];
	for (const [name, value, path] of cases) {
		const result = validate('DescribeResponse', value);
		assert.equal(result.ok, false, name);
		if (!result.ok) assert.ok(result.errors.some((error) => error.startsWith(path)), `${name}: ${result.errors.join('; ')}`);
	}
});

test('header fields and the legacy map validate on HandleResponse', () => {
	const result = validate('HandleResponse', {
		status: 200,
		headers: { 'set-cookie': 'a=1\nb=2' },
		headerFields: [{ name: 'set-cookie', values: ['a=1; Expires=Wed, 21 Oct 2026 07:28:00 GMT', 'b=2'] }],
		body: '',
	});
	assert.ok(result.ok);
	assert.deepEqual(result.value.headerFields?.[0]?.values, ['a=1; Expires=Wed, 21 Oct 2026 07:28:00 GMT', 'b=2']);
	assert.equal(validate('HandleResponse', { headers: { a: 1 } }).ok, false);
});

test('the schema covers every enum a message refers to', () => {
	for (const [name, fields] of Object.entries(CONTRACT_SCHEMA.messages)) {
		for (const field of fields) {
			if (field.kind === 'enum') assert.ok(CONTRACT_SCHEMA.enums[field.type ?? ''], `${name}.${field.json}`);
			if (field.kind === 'message') assert.ok(field.type && field.type in CONTRACT_SCHEMA.messages, `${name}.${field.json}`);
		}
	}
});
