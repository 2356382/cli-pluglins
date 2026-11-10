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

int main() {
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

    printf("\n=== plugin log content ===\n");
    fflush(stdout);
    int rc = system("cat " LOG_PATH);
    (void)rc;
    return 0;
}
