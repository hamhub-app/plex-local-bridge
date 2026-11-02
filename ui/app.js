(function () {
    'use strict';
    const $ = id => document.getElementById(id);
    const managementRequest = PlexLocalBridgeSession.createClient(window.fetch.bind(window));
    let current = null, dirty = false, busy = false, bootLoaded = false;
    function message(text, error) { $('notice').textContent = text; $('notice').className = 'notice' + (error ? ' error' : ''); $('notice').hidden = !text; }
    function errorText(error) { return error && error.message || (error && error.code ? 'DSM 错误码 ' + error.code : '操作失败，请刷新状态后重试'); }
    function driver() { return window.PlexLocalBridgeDesktop; }
    function controls() {
        $('save').disabled = busy || !current;
        $('activate').disabled = busy || !current || !driver();
        $('stop-gateway').disabled = busy || !current || !driver();
        $('boot-recovery').disabled = busy || !driver();
        $('defaults').disabled = busy;
        $('desktop-hint').hidden = Boolean(driver());
    }
    async function api(action, data) {
        const options = {credentials: 'same-origin', cache: 'no-store', headers: {Accept: 'application/json'}};
        if (data) { options.method = 'POST'; options.headers['Content-Type'] = 'application/json'; options.headers['X-PLB-CSRF'] = current.csrf; options.body = JSON.stringify(data); }
        const response = await managementRequest(action, options);
        let result;
        try { result = await response.json(); } catch (_) { throw new Error('DSM 管理接口返回 HTTP ' + response.status + '，未能启动或返回 JSON 数据，请检查 CGI 运行环境'); }
        if (!response.ok || result.error) throw new Error(result.error || '管理请求失败（' + response.status + '）');
        return result;
    }
    function fill(config) {
        $('public-port').value = config.public_port;
        $('listen-port').value = config.listen_port;
        $('auto-upstream').checked = config.upstream_url === 'auto';
        $('upstream-url').value = config.upstream_url === 'auto' ? '' : config.upstream_url;
        $('upstream-url').disabled = $('auto-upstream').checked;
        $('intercept-ipv6').checked = config.intercept_ipv6;
        $('max-connections').value = config.max_connections;
    }
    function desired() { return {upstream_url: $('auto-upstream').checked ? 'auto' : $('upstream-url').value.trim(),
        public_port: Number($('public-port').value), listen_port: Number($('listen-port').value),
        intercept_ipv6: $('intercept-ipv6').checked, max_connections: Number($('max-connections').value)}; }
    function badge(id, text, kind) { $(id).textContent = text; $(id).className = 'badge ' + kind; }
    function render(state) {
        current = state; $('version').textContent = state.version;
        if (!dirty) fill(state.config);
        badge('proxy-badge', state.proxy_running ? '运行中' : '未就绪', state.proxy_running ? 'good' : 'bad');
        $('proxy-detail').textContent = state.proxy_running ? '监听端口 ' + state.config.listen_port + ' · Plex 账号鉴权已保留' : state.proxy_error || '请检查下方诊断信息';
        const gateway = state.gateway || {}, phases = {active: ['接管中', 'good'], waiting: ['等待代理', 'warn'], preparing: ['准备中', 'warn'],
            failed: ['启用失败', 'bad'], stopped: ['已停用', 'neutral'], stale: ['状态过期', 'warn'], unavailable: ['尚未确认', 'neutral']};
        const phase = phases[gateway.phase] || phases.unavailable; badge('gateway-badge', phase[0], phase[1]);
        $('gateway-detail').textContent = gateway.phase === 'active' && gateway.config ? '已授权入口 ' + gateway.config.public_port + ' → ' + gateway.config.listen_port : gateway.message || state.gateway_error || '等待管理员授权';
        $('flow').textContent = state.config.public_port + ' → ' + state.config.listen_port + ' → Plex';
        $('approval-message').textContent = state.approval_needed ? '当前配置尚未获得接管授权。保存后点击“启用 / 更新接管”，在 DSM 身份确认中完成授权。' : '当前配置与管理员批准的副本一致。修改入口、监听端口或上游后，需要更新授权。';
        const errors = [state.config_error, gateway.phase === 'failed' || gateway.phase === 'stale' ? gateway.message : '', ...(state.recent_errors || [])].filter(Boolean);
        $('diagnostics').hidden = !errors.length; $('diagnostic-text').textContent = errors.join('\n\n');
        controls();
    }
    async function refresh() { const state = await api('status'); render(state); return state; }
    async function desktopReady() {
        controls();
        if (!driver() || bootLoaded) return;
        try { $('boot-recovery').checked = await driver().bootEnabled(); bootLoaded = true; }
        catch (error) { message('开机任务状态读取失败：' + errorText(error), true); }
    }
    async function waitGateway(active, previousGateway) {
        const until = Date.now() + 45000;
        while (Date.now() < until) {
            const state = await refresh(), phase = state.gateway && state.gateway.phase;
            const fresh = state.gateway && (active ? state.gateway.run_id !== previousGateway.run_id : state.gateway.updated_at !== previousGateway.updated_at || previousGateway.phase === 'stopped');
            if (fresh && active && phase === 'active' && !state.approval_needed) return;
            if (fresh && !active && phase === 'stopped') return;
            if (fresh && phase === 'failed') throw new Error(state.gateway.message);
            await new Promise(resolve => setTimeout(resolve, 1500));
        }
        throw new Error('DSM 已提交操作，但尚未确认接管结果，请查看诊断信息');
    }
    async function action(operation) {
        if (busy) return; busy = true; controls(); message('正在处理…');
        try { await operation(); } catch (error) { message(errorText(error), true); }
        finally { busy = false; controls(); }
    }
    $('settings-form').addEventListener('input', function () { dirty = true; });
    $('auto-upstream').addEventListener('change', function () { $('upstream-url').disabled = this.checked; });
    $('settings-form').addEventListener('submit', function (event) { event.preventDefault(); action(async function () {
        const result = await api('save', {config: desired(), revision: current.revision}); dirty = false; render(result);
        message(result.approval_needed ? '设置已保存，代理已重启。接下来点击“启用 / 更新接管”确认新的入口配置。' : '设置已保存并应用。');
    }); });
    $('activate').addEventListener('click', function () { action(async function () {
        if (dirty) throw new Error('请先保存连接设置，再授权端口接管');
        const previousGateway = current.gateway || {};
        message('请在 DSM 身份确认窗口中完成授权…');
        const result = await driver().activate({boot: $('boot-recovery').checked});
        message('授权操作已提交，正在确认端口接管结果…'); await waitGateway(true, previousGateway);
        message(result.bootError || '端口接管已生效，可以继续使用原来的 Plex 入口。', Boolean(result.bootError));
    }); });
    $('stop-gateway').addEventListener('click', function () { action(async function () {
        const previousGateway = current.gateway || {};
        message('请在 DSM 身份确认窗口中确认停用…'); await driver().stop(); await waitGateway(false, previousGateway);
        $('boot-recovery').checked = false; message('端口接管已停用，原生 Plex 入口已恢复。');
    }); });
    $('defaults').addEventListener('click', function () { fill({upstream_url: 'auto', public_port: 32400, listen_port: 13240, intercept_ipv6: true, max_connections: 256}); dirty = true; message('已填入默认值，点击“保存并应用”后才会生效。'); });
    $('refresh').addEventListener('click', function () { action(async function () { await refresh(); message('状态已刷新。'); }); });
    window.addEventListener('plb-dsm-ready', desktopReady);
    refresh().then(desktopReady).catch(error => { message(errorText(error), true); controls(); });
    setInterval(function () { if (!busy) refresh().catch(error => message(errorText(error), true)); }, 10000);
}());
