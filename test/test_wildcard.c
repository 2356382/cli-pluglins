#include <stdio.h>
#include <string.h>
#include "cli-runner.h"

static void call(const char *method, const char *req) {
    cliproxy_buffer resp;
    int rc = cliproxyPluginCall((char*)method, (unsigned char*)req, req ? strlen(req) : 0, &resp);
    printf("\n[%s]\nrc=%d\n", method, rc);
    if (resp.ptr) {
        printf("resp: %.*s\n", (int)resp.len, (char*)resp.ptr);
        cliproxyPluginFree(resp.ptr, resp.len);
    }
}

int main() {
    // 1. 默认状态（不带配置注册）：应允许所有命令
    call("plugin.register", "{}");
    // 默认通配下执行 sw_vers（macOS 专属命令，不在旧白名单里）
    call("command_line.execute",
         "{\"Program\":\"cliproxyapi\",\"Flags\":{\"run-local-cli\":{\"Name\":\"run-local-cli\",\"Type\":\"string\",\"Value\":\"sw_vers\",\"Set\":true}}}");
    // 默认通配下写入任意路径（旧逻辑会拒绝 /root/lib，因无权限；用 /tmp 下的新目录验证放行）
    call("command_line.execute",
         "{\"Program\":\"cliproxyapi\",\"Flags\":{\"write-data\":{\"Name\":\"write-data\",\"Type\":\"string\",\"Value\":\"/Users/c16001/lxl/github/cli-pluglins/dist/wildcard-test.txt=anywhere\",\"Set\":true}}}");

    // 2. 收紧配置：白名单只允许 ls，write_dir 限定 /tmp
    call("plugin.reconfigure",
         "{\"config_yaml\":\"allowed_commands: ls\\nwrite_dir: /tmp\\n\"}");
    // rm 应被拒绝
    call("command_line.execute",
         "{\"Program\":\"cliproxyapi\",\"Flags\":{\"run-local-cli\":{\"Name\":\"run-local-cli\",\"Type\":\"string\",\"Value\":\"rm --help\",\"Set\":true}}}");
    // 目录外写入应被拒绝
    call("command_line.execute",
         "{\"Program\":\"cliproxyapi\",\"Flags\":{\"write-data\":{\"Name\":\"write-data\",\"Type\":\"string\",\"Value\":\"/Users/c16001/lxl/github/cli-pluglins/dist/blocked.txt=nope\",\"Set\":true}}}");

    // 3. 重新放开：allowed_commands: * 且 write_dir: all
    call("plugin.reconfigure",
         "{\"config_yaml\":\"allowed_commands: *\\nwrite_dir: all\\n\"}");
    call("command_line.execute",
         "{\"Program\":\"cliproxyapi\",\"Flags\":{\"run-local-cli\":{\"Name\":\"run-local-cli\",\"Type\":\"string\",\"Value\":\"whoami\",\"Set\":true}}}");

    printf("\n=== Done ===\n");
    return 0;
}
