const test = require('node:test');
const assert = require('node:assert/strict');
const {createClient} = require('../ui/dsm-session.js');

test('DSM token stays in memory and only goes in the fixed management request header', async () => {
    const calls = [];
    const request = createClient(async (url, options) => {
        calls.push({url, options});
        return url === '/webman/login.cgi' ? {ok: true, json: async () => ({SynoToken: 'fixture-session'})} : {ok: true, status: 200};
    });
    await request('status', {headers: {Accept: 'application/json'}});
    await request('save', {method: 'POST', headers: {'X-PLB-CSRF': 'fixture-csrf'}, body: '{}'});
    assert.equal(calls.length, 3);
    assert.equal(calls[1].options.headers['X-SYNO-TOKEN'], 'fixture-session');
    assert.equal(calls[2].options.headers['X-PLB-CSRF'], 'fixture-csrf');
    assert.equal(calls[2].options.credentials, 'same-origin');
    assert.ok(calls.every(c => !c.url.includes('fixture-session')));
    await assert.rejects(request('https://elsewhere.example', {}), /未知管理操作/);
    assert.equal(calls.length, 3, 'foreign destinations must never receive the DSM token');
});

test('a rejected session refreshes DSM token once before returning the result', async () => {
    let logins = 0, management = 0;
    const request = createClient(async (url, options) => {
        if (url === '/webman/login.cgi') {logins++; return {ok: true, json: async () => ({SynoToken: 'fixture-' + logins})};}
        management++;
        assert.equal(options.headers['X-SYNO-TOKEN'], 'fixture-' + management);
        return {ok: management > 1, status: management > 1 ? 200 : 401};
    });
    assert.equal((await request('status', {headers: {}})).status, 200);
    assert.equal(logins, 2); assert.equal(management, 2);
});
