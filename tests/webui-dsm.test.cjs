const test = require('node:test');
const assert = require('node:assert/strict');
const {createDriver} = require('../ui/dsm-bridge.js');

function fixture() {
    const calls = [], tasks = [], boot = [];
    let confirmations = 0, nextID = 1;
    const request = async spec => {
        calls.push(structuredClone(spec));
        if (spec.api === 'SYNO.Core.TaskScheduler' && spec.method === 'list') return {tasks, total: tasks.length};
        if (spec.api === 'SYNO.Core.TaskScheduler' && spec.method === 'get') return tasks.find(t => t.id === spec.params.id);
        if (spec.api === 'SYNO.Core.TaskScheduler.Root') {
            assert.equal(spec.params.SynoConfirmPWToken, 'fixture-confirmation');
            if (spec.method === 'create') { const task = {...structuredClone(spec.params), id: nextID++}; tasks.push(task); return {id: task.id}; }
            Object.assign(tasks.find(t => t.id === spec.params.id), structuredClone(spec.params)); return {};
        }
        if (spec.api === 'SYNO.Core.TaskScheduler' && spec.method === 'run') return {};
        if (spec.api === 'SYNO.Core.EventScheduler' && spec.method === 'list') return boot;
        if (spec.api === 'SYNO.Core.EventScheduler' && spec.method === 'get') return boot[0];
        if (spec.api === 'SYNO.Core.EventScheduler.Root') { boot[0] = structuredClone(spec.params); return {}; }
        throw new Error('Unexpected request');
    };
    const driver = createDriver(request, async () => {confirmations++; return 'fixture-confirmation';});
    return {calls, tasks, boot, request, driver, confirmations: () => confirmations};
}

test('native confirmation creates only a disabled fixed authorization task', async () => {
    const f = fixture();
    await f.driver.activate({script: 'untrusted text is ignored'});
    assert.equal(f.confirmations(), 1);
    assert.equal(f.tasks.length, 1);
    assert.equal(f.tasks[0].enable, false);
    assert.equal(f.tasks[0].extra.script, '/bin/sh /var/packages/PlexLocalBridge/target/admin/gatewayctl.sh enable');
    assert.equal(f.calls.filter(s => s.method === 'run').length, 1);
    await f.driver.activate();
    assert.equal(f.tasks.length, 1, 'retry must not create duplicate tasks');
    assert.equal(f.confirmations(), 2, 'updating authorization requires a fresh confirmation');
});

test('canceling the native password confirmation performs no task or configuration change', async () => {
    const f = fixture(); let saved = false;
    const d = createDriver(f.request, async () => '');
    await assert.rejects(d.activate({before: async () => {saved = true;}}), /已取消/);
    assert.equal(saved, false); assert.deepEqual(f.calls, []);
});

test('same-name foreign tasks are never overwritten or run', async () => {
    const f = fixture();
    f.tasks.push({name: 'PlexLocalBridge-Authorize', id: 9, owner: 'root', type: 'script', extra: {script: 'unrelated command'}});
    await assert.rejects(f.driver.activate(), /拒绝/);
    assert.equal(f.calls.filter(s => s.method === 'run' || s.api.endsWith('.Root')).length, 0);
});

test('duplicate task names fail without changing either task', async () => {
    const f = fixture(); f.tasks.push({name: 'PlexLocalBridge-Authorize'}, {name: 'PlexLocalBridge-Authorize'});
    await assert.rejects(f.driver.activate(), /多个同名/);
    assert.equal(f.calls.filter(s => s.api.endsWith('.Root')).length, 0);
});

test('boot recovery only executes the administrator-protected copy', async () => {
    const f = fixture(); await f.driver.activate({boot: true});
    assert.equal(f.boot[0].owner['0'], 'root'); assert.equal(f.boot[0].event, 'bootup');
    assert.equal(f.boot[0].operation, '/usr/local/libexec/plex-local-bridge/gatewayctl start');
    assert.equal(await f.driver.bootEnabled(), true);
    await f.driver.stop(); assert.equal(f.boot[0].enable, false);
    const stop = f.tasks.find(t => t.name === 'PlexLocalBridge-Stop');
    assert.equal(stop.extra.script, '/usr/local/libexec/plex-local-bridge/gatewayctl stop');
});

test('a foreign boot task is not modified', async () => {
    const f = fixture(); f.boot.push({task_name: 'PlexLocalBridge-Startup', owner: {'0': 'root'}, operation_type: 'script', operation: 'unrelated'});
    await assert.rejects(f.driver.bootEnabled(), /拒绝修改/);
    assert.equal(f.calls.filter(s => s.api === 'SYNO.Core.EventScheduler.Root').length, 0);
});

test('partial boot configuration failure is reported without hiding queued activation', async () => {
    const f = fixture();
    const request = async spec => {
        if (spec.api === 'SYNO.Core.EventScheduler.Root') throw {code: 119};
        return f.request(spec);
    };
    const d = createDriver(request, async () => 'fixture-confirmation');
    const result = await d.activate({boot: true});
    assert.match(result.bootError, /119/);
    assert.equal(f.calls.filter(s => s.method === 'run').length, 1);
});

test('desktop password verification uses DSM encryption and forgets a canceled confirmation', async () => {
    const fs = require('node:fs'), vm = require('node:vm');
    const specs = [];
    let confirm, component, canceled = false;
    const context = {
        SYNO: {namespace() {}, SDS: {PlexLocalBridge: {}}},
        Vue: {extend(value) {component = value; return value;}}, Event,
        PlexLocalBridgeDSM: {createDriver(request, callback) {confirm = callback; return {}; }},
        window: {synowebapi: {promises: {request: async spec => {
            specs.push(spec);
            // DSM 的 v1 只返回 success，SDK 解包后没有凭证；不能用于特权任务授权。
            return spec.version >= 2 ? {SynoConfirmPWToken: 'fixture-token'} : undefined;
        }}}}
    };
    vm.runInNewContext(fs.readFileSync(require.resolve('../ui/desktop.js'), 'utf8'), context);
    const frame = {addEventListener() {}, contentWindow: {dispatchEvent() {}}};
    const instance = {$refs: {page: frame, appWindow: {getMsgBox() {return {
        async password(title, message, buttons, options) {
            if (canceled) return 'cancel';
            await options.prompt.verify('fixture-password'); return 'confirm';
        }
    };}}}};
    component.mounted.call(instance);
    assert.equal(await confirm({title: 'test', message: 'test'}), 'fixture-token');
    assert.equal(specs[0].api, 'SYNO.Core.User.PasswordConfirm');
    assert.equal(specs[0].version, 2);
    assert.equal(specs[0].encryption[0], 'password');
    assert.equal(specs[0].params.password, 'fixture-password');
    canceled = true;
    assert.equal(await confirm({title: 'test', message: 'test'}), '');
    assert.equal(specs.length, 1, 'cancel must not reuse the previous confirmation');
});
