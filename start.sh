#!/bin/bash

set -e

MYSQL_CONTAINER_NAME="mysql-container"
MYSQL_ROOT_PASSWORD="1234"
MYSQL_DATABASE="golang"
MYSQL_PORT="3306"
APP_CONTAINER_NAME="go-app"
APP_PORT="8080"

echo "Stopping and removing any existing containers..."
docker stop $MYSQL_CONTAINER_NAME $APP_CONTAINER_NAME || true
docker rm $MYSQL_CONTAINER_NAME $APP_CONTAINER_NAME || true

echo "Starting MySQL container..."
docker run --name $MYSQL_CONTAINER_NAME \
  -e MYSQL_ROOT_PASSWORD=$MYSQL_ROOT_PASSWORD \
  -e MYSQL_DATABASE=$MYSQL_DATABASE \
  -p $MYSQL_PORT:3306 \
  -d mysql:latest

echo "Waiting for MySQL to initialize..."
until docker exec $MYSQL_CONTAINER_NAME mysqladmin ping -h "127.0.0.1" --silent; do
  sleep 1
done

echo "Initializing database schema..."
docker exec -i $MYSQL_CONTAINER_NAME mysql -u root -p$MYSQL_ROOT_PASSWORD $MYSQL_DATABASE < schema.sql

echo "Building Go application Docker image..."
docker build -t go-app .

echo "Starting Go application container..."
docker run --name $APP_CONTAINER_NAME \
  --link $MYSQL_CONTAINER_NAME:mysql \
  -p $APP_PORT:8080 \
  -d go-app

echo "Application is running on http://localhost:$APP_PORT"
