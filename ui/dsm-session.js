/* 管理请求复用当前 DSM 会话凭证，仅保存在页面内存并发送到本套件的固定接口。 */
(function (root, factory) {
    if (typeof module === 'object' && module.exports) module.exports = factory();
    else root.PlexLocalBridgeSession = factory();
}(typeof globalThis !== 'undefined' ? globalThis : this, function () {
    'use strict';
    function createClient(fetcher) {
        let session = null;
        async function token() {
            if (!session) session = (async function () {
                const response = await fetcher('/webman/login.cgi', {credentials: 'same-origin', cache: 'no-store', headers: {Accept: 'application/json'}});
                if (!response.ok) throw new Error('无法读取 DSM 登录状态，请刷新 DSM 后重新打开套件');
                const data = await response.json();
                return typeof data.SynoToken === 'string' ? data.SynoToken : '';
            }()).catch(error => {session = null; throw error;});
            return session;
        }
        return async function request(action, options) {
            if (action !== 'status' && action !== 'save') throw new Error('未知管理操作');
            async function send() {
                const value = await token();
                const headers = Object.assign({}, options.headers);
                if (value) headers['X-SYNO-TOKEN'] = value;
                return fetcher('api.cgi?action=' + action, Object.assign({}, options, {credentials: 'same-origin', cache: 'no-store', headers}));
            }
            let response = await send();
            if (response.status === 401) {session = null; response = await send();}
            return response;
        };
    }
    return {createClient};
}));
