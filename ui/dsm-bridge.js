/* DSM 授权流程只接受固定操作，不接受页面提供的任意命令或任务名称。 */
(function (root, factory) {
    if (typeof module === 'object' && module.exports) module.exports = factory();
    else root.PlexLocalBridgeDSM = factory();
}(typeof globalThis !== 'undefined' ? globalThis : this, function () {
    'use strict';
    const ENABLE_NAME = 'PlexLocalBridge-Authorize';
    const STOP_NAME = 'PlexLocalBridge-Stop';
    const BOOT_NAME = 'PlexLocalBridge-Startup';
    const ENABLE = '/bin/sh /var/packages/PlexLocalBridge/target/admin/gatewayctl.sh enable';
    const STOP = '/usr/local/libexec/plex-local-bridge/gatewayctl stop';
    const START = '/usr/local/libexec/plex-local-bridge/gatewayctl start';
    function errorText(error) { return error && error.message || (error && error.code ? 'DSM 错误码 ' + error.code : 'DSM 未完成操作'); }

    function createDriver(request, confirm) {
        async function taskNamed(name) {
            let found = [];
            for (let offset = 0; offset < 4000; offset += 200) {
                const page = await request({api: 'SYNO.Core.TaskScheduler', method: 'list', version: 3,
                    params: {offset, limit: 200, sort_by: 'name', sort_direction: 'ASC'}});
                const tasks = page.tasks || [];
                found = found.concat(tasks.filter(task => task.name === name));
                if (tasks.length < 200 || offset + tasks.length >= page.total) break;
                if (offset === 3800) throw new Error('任务数量超过安全检查范围，请先在 DSM 任务计划中检查');
            }
            if (found.length > 1) throw new Error('存在多个同名接管任务，请在 DSM 任务计划中检查');
            return found[0];
        }

        async function runFixedTask(name, script, token) {
            const existing = await taskNamed(name);
            if (existing) {
                const details = await request({api: 'SYNO.Core.TaskScheduler', method: 'get', version: 4,
                    params: {id: existing.id, real_owner: existing.real_owner || existing.owner}});
                if (details.owner !== 'root' || details.type !== 'script' || !details.extra || details.extra.script !== script) {
                    throw new Error('同名任务内容与本套件不符，已拒绝覆盖或运行');
                }
            }
            const params = {name, owner: 'root', real_owner: 'root', enable: false, type: 'script',
                schedule: {date_type: 0, week_day: '0,1,2,3,4,5,6', repeat_date: 1001, monthly_week: [],
                    hour: 0, minute: 0, repeat_hour: 0, repeat_min: 0, last_work_hour: 0,
                    repeat_min_store_config: [1,5,10,15,20,30], repeat_hour_store_config: Array.from({length:23}, (_,i) => i+1)},
                extra: {script, notify_enable: false, notify_mail: '', notify_if_error: false}, SynoConfirmPWToken: token};
            if (existing) params.id = existing.id;
            const result = await request({api: 'SYNO.Core.TaskScheduler.Root', method: existing ? 'set' : 'create', version: 4, params});
            const id = existing ? existing.id : result && result.id;
            if (id === undefined || id === null) throw new Error('DSM 未返回授权任务编号，未继续运行');
            await request({api: 'SYNO.Core.TaskScheduler', method: 'run', version: 2,
                params: {tasks: [{id, real_owner: 'root'}]}});
        }

        async function bootTask() {
            const tasks = await request({api: 'SYNO.Core.EventScheduler', method: 'list', version: 1, params: {event: 'bootup'}});
            if (!Array.isArray(tasks)) throw new Error('DSM 返回了未知的开机任务格式');
            const matches = tasks.filter(task => task.task_name === BOOT_NAME);
            if (matches.length > 1) throw new Error('存在重复的开机接管任务');
            if (!matches.length) return null;
            const details = await request({api: 'SYNO.Core.EventScheduler', method: 'get', version: 1, params: {task_name: BOOT_NAME}});
            if (!details.owner || details.owner['0'] !== 'root' || details.operation_type !== 'script' || details.operation !== START) {
                throw new Error('同名开机任务不是本套件的固定操作，已拒绝修改');
            }
            return details;
        }

        async function setBoot(enabled, token) {
            const existing = await bootTask();
            if (!existing && !enabled) return;
            await request({api: 'SYNO.Core.EventScheduler.Root', method: existing ? 'set' : 'create', version: 1,
                params: {task_name: BOOT_NAME, owner: {'0': 'root'}, event: 'bootup', enable: enabled,
                    operation_type: 'script', operation: START, depend_on_task: '', SynoConfirmPWToken: token}});
        }

        async function authorize(operation, before) {
            let token = await confirm(operation);
            if (!token) throw new Error('已取消管理员授权，未执行端口接管');
            try {
                if (before) await before();
                return await operation.run(token);
            } finally { token = ''; }
        }

        return {
            async activate(options) {
                options = options || {};
                return authorize({title: '授权 Plex 端口接管',
                    message: '将按当前配置接管 NAS 的 Plex 入口，并加载所需的系统 NAT 模块。管理员密码只交给 DSM 校验。',
                    run: async token => {
                        await runFixedTask(ENABLE_NAME, ENABLE, token);
                        try { await setBoot(Boolean(options.boot), token); }
                        catch (error) { return {bootError: '接管启用命令已提交，但开机设置未完成：' + errorText(error)}; }
                        return {};
                    }}, options.before);
            },
            async stop() {
                return authorize({title: '停用 Plex 端口接管', message: '恢复原生 Plex 入口，并停用本套件的开机恢复任务。',
                    run: async token => {
                        await setBoot(false, token);
                        await runFixedTask(STOP_NAME, STOP, token);
                        return {};
                    }});
            },
            async bootEnabled() {
                const task = await bootTask();
                return Boolean(task && (task.enable === true || task.enable === 'true'));
            }
        };
    }
    return {createDriver};
}));
