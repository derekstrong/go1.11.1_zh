#!/usr/bin/env python3

import os
import subprocess
import sys

# 检查的 Python 文件目录
CODE_DIR = "src"

# 检查命令
PYLINT_CMD = "pylint"

# 需要忽略的检查规则
PYLINT_IGNORE = "W,C,R"


def check_code():
    # 构造检查命令
    cmd = [
        PYLINT_CMD,
        "--jobs=0",
        "--ignore={}".format(PYLINT_IGNORE),
        CODE_DIR,
    ]
    
    # 运行检查命令
    try:
        subprocess.check_call(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except subprocess.CalledProcessError as exc:
        # 检查失败，输出错误信息并返回非零状态码
        print("Code check failed: ", exc.stderr.decode("utf-8"))
        sys.exit(1)


if __name__ == "__main__":
    check_code()
