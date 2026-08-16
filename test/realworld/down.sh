#!/bin/bash
docker rm -f anetlink-ha anetlink-mosq anetlink-modbus anetlink-matter anetlink-opcua 2>/dev/null || true
