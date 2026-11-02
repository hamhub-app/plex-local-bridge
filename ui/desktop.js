/* DSM 原生窗口承载管理页面，密码确认和特权任务均由 DSM 原生组件完成。 */
SYNO.namespace('SYNO.SDS.PlexLocalBridge');
SYNO.SDS.PlexLocalBridge.Instance = Vue.extend({
    template: '<v-app-instance class-name="SYNO.SDS.PlexLocalBridge.Instance">' +
        '<v-app-window ref="appWindow" syno-id="SYNO.SDS.PlexLocalBridge.Window" width="1100" height="820" :resizable="true">' +
        '<iframe ref="page" :src="src" title="Plex Local Bridge" style="display:block;width:100%;height:100%;border:0;background:#f4f6f9"></iframe>' +
        '</v-app-window></v-app-instance>',
    data: function () {return {src: '/webman/3rdparty/PlexLocalBridge/index.html?t=' + Date.now()};},
    mounted: function () {
        const self = this;
        const request = function (spec) {
            if (!window.synowebapi || !window.synowebapi.promises) return Promise.reject(new Error('当前 DSM 未提供任务授权接口'));
            return window.synowebapi.promises.request(spec);
        };
        const confirm = async function (operation) {
            const appWindow = self.$refs.appWindow;
            if (!appWindow || !appWindow.getMsgBox) throw new Error('当前 DSM 无法打开原生身份确认窗口');
            let token = '';
            const response = await appWindow.getMsgBox().password(operation.title, operation.message, undefined, {
                prompt: {label: '当前 DSM 管理员密码：', verify: async function (password) {
                    // v1 只确认密码成功；创建 root 任务需要 v2 返回的短期确认凭证。
                    const result = await request({api: 'SYNO.Core.User.PasswordConfirm', method: 'auth', version: 2,
                        encryption: ['password'], params: {password: password}});
                    token = result && result.SynoConfirmPWToken || '';
                    if (!token) throw new Error('DSM 未返回本次确认凭证');
                    return true;
                }}
            });
            if (response !== 'confirm') return '';
            return token;
        };
        this.driver = PlexLocalBridgeDSM.createDriver(request, confirm);
        this.attachBridge = function () {
            const frame = self.$refs.page;
            if (!frame || !frame.contentWindow) return;
            frame.contentWindow.PlexLocalBridgeDesktop = self.driver;
            frame.contentWindow.dispatchEvent(new Event('plb-dsm-ready'));
        };
        this.$refs.page.addEventListener('load', this.attachBridge);
        this.attachBridge();
    },
    beforeDestroy: function () {
        if (this.$refs.page && this.attachBridge) this.$refs.page.removeEventListener('load', this.attachBridge);
    }
});
