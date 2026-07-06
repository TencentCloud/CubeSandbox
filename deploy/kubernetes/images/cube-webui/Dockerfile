FROM cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat

COPY package/webui/dist /usr/share/nginx/html
COPY package/webui/nginx.conf /usr/local/openresty/nginx/conf/nginx.conf

EXPOSE 80

CMD ["/usr/local/openresty/bin/openresty", "-g", "daemon off;"]
