#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "cli-runner.h"

static void call(const char *method, const char *req) {
    cliproxy_buffer resp;
    int rc = cliproxyPluginCall((char*)method, (unsigned char*)req, req ? strlen(req) : 0, &resp);
    printf("[%s] rc=%d\n", method, rc);
    if (resp.ptr) {
        cliproxyPluginFree(resp.ptr, resp.len);
    }
}

static int sh(const char *cmd) {
    fflush(stdout);
    int rc = system(cmd);
    return rc == 0;
}

int main() {
    // 清理旧日志，保证断言只针对本次运行。
    system("rm -f " LOG_PATH);

    // 1. 注册（带配置：开 write_enabled，写入测试路径）
    call("plugin.register",
         "{\"config_yaml\":\"write_enabled: true\\nwrite_path: /tmp/cli-runner-log-test.txt\\nwrite_content: logged\\n\"}");
    // 2. 执行命令
    call("command_line.execute",
         "{\"Program\":\"cliproxyapi\",\"Flags\":{\"run-local-cli\":{\"Name\":\"run-local-cli\",\"Type\":\"string\",\"Value\":\"whoami\",\"Set\":true}}}");
    // 3. write-data
    call("command_line.execute",
         "{\"Program\":\"cliproxyapi\",\"Flags\":{\"write-data\":{\"Name\":\"write-data\",\"Type\":\"string\",\"Value\":\"/tmp/cli-runner-log-test2.txt=data\",\"Set\":true}}}");
    // 4. 白名单拒绝（先收紧）
    call("plugin.reconfigure", "{\"config_yaml\":\"allowed_commands: ls\\n\"}");
    call("command_line.execute",
         "{\"Program\":\"cliproxyapi\",\"Flags\":{\"run-local-cli\":{\"Name\":\"run-local-cli\",\"Type\":\"string\",\"Value\":\"rm --help\",\"Set\":true}}}");

    printf("\n=== plugin log content (" LOG_PATH ") ===\n");
    sh("cat " LOG_PATH);

    // 5. 校验宿主 main.log 行格式：
    //    [2006-01-02 15:04:05] [--------] [info ] cli-runner: <msg> plugin_id=cli-runner
    printf("\n=== format checks ===\n");
    int ok = 1;
    ok &= sh("grep -qE '^\\[[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\\] \\[--------\\] \\[info \\] cli-runner: ' " LOG_PATH);
    ok &= sh("grep -q '\\[warn \\] cli-runner: cmd DENIED' " LOG_PATH);
    ok &= sh("grep -q 'plugin_id=cli-runner$' " LOG_PATH);
    ok &= sh("grep -q 'exec: whoami' " LOG_PATH);
    printf("%s\n", ok ? "FORMAT-OK" : "FORMAT-FAIL");

    // 6. 验证宿主默认路径解析：未设 CLIPROXY_CLI_RUNNER_LOG 时，
    //    WRITABLE_PATH 环境变量 → {WRITABLE_PATH}/logs/main.log（复刻宿主 ResolveLogDirectory）。
    unsetenv("CLIPROXY_CLI_RUNNER_LOG");
    setenv("WRITABLE_PATH", "/tmp/cliproxy-wp-test", 1);
    system("rm -rf /tmp/cliproxy-wp-test");
    call("plugin.reconfigure", "{\"config_yaml\":\"write_enabled: false\\n\"}");
    printf("\n=== resolved host main.log (/tmp/cliproxy-wp-test/logs/main.log) ===\n");
    int ok2 = sh("cat /tmp/cliproxy-wp-test/logs/main.log");
    if (ok2) ok2 &= sh("grep -q 'config applied' /tmp/cliproxy-wp-test/logs/main.log");
    printf("%s\n", ok2 ? "RESOLVE-OK" : "RESOLVE-FAIL");

    return (ok && ok2) ? 0 : 1;
}
