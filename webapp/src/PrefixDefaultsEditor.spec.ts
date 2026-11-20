import {expect, test} from '@playwright/test';

import {parseDefaults, parseMembers, serializeDefaults} from './PrefixDefaultsEditor';

test.describe('prefix defaults helpers', () => {
    test('parseDefaults reads saved JSON and ignores anything else', () => {
        expect(parseDefaults('')).toEqual({});
        expect(parseDefaults('{oops')).toEqual({});
        expect(parseDefaults('[1]')).toEqual({});
        expect(parseDefaults('{"sec-": {"channel_type": "P", "members": ["a", 3]}, "x-": 5}')).toEqual({
            'sec-': {channel_type: 'P', purpose: '', header: '', members: ['a']},
        });
    });

    test('parseMembers splits, strips @ and removes repeats', () => {
        expect(parseMembers(' @Alice, bob  alice,,')).toEqual(['alice', 'bob']);
        expect(parseMembers('')).toEqual([]);
    });

    test('serializeDefaults keeps only set fields in a stable order', () => {
        expect(serializeDefaults({})).toBe('');
        expect(serializeDefaults({'z-': {channel_type: '', purpose: ' ', header: '', members: []}})).toBe('');
        expect(serializeDefaults({
            'sec-': {channel_type: 'P', purpose: ' SOC ', header: '', members: ['alice']},
            'ops-': {header: 'Runbook'},
        })).toBe('{"ops-":{"header":"Runbook"},"sec-":{"channel_type":"P","purpose":"SOC","members":["alice"]}}');
    });
});
