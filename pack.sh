
[ -f build ] && rm -rf build 
mkdir -p build

# 静态编译 - 避免 GLIBC 版本依赖问题
# CGO_ENABLED=0 禁用 CGO，生成静态链接二进制文件
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o build/chatAgent main.go

cp -r config build/
rm -rf build/config/sessions
