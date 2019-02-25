# Copyright 2016 The Kubernetes Authors All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# network performance tests in containers
#
# Run as the Orchestrator: Arguments: --mode=orchestrator
#
# Run as a Worker (first get orchestrator virtual IP address):
# kubectl get svc --format "{{ .NetworkSettings.IPAddress }}" netperf-orch
#
# Args: --mode=worker --host=<service cluster ip> --port=5202
#
FROM fortio/fortio:1.3.0 as fortiobuild


FROM debian:jessie as netperfbuild

MAINTAINER Kolesza Renáta <kolreny@gmail.com>

# install binary and remove cache
RUN apt-get update \
    && apt-get install -y iperf3 curl wget net-tools gcc make \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /tmp

# Download and build netperf from sources
RUN curl -LO https://github.com/HewlettPackard/netperf/archive/netperf-2.7.0.tar.gz && tar -xzf netperf-2.7.0.tar.gz && mv netperf-netperf-2.7.0/ netperf-2.7.0
RUN cd netperf-2.7.0 && ./configure --prefix=/usr/local --bindir /usr/local/bin && make && make install

FROM debian:jessie

RUN apt-get update \
    && apt-get install -y iperf3 curl wget net-tools gcc make \
    && rm -rf /var/lib/apt/lists/*

# Copy netperf binarias from the built image
COPY --from=netperfbuild /usr/local/bin/netperf /usr/local/bin/netperf
COPY --from=netperfbuild /usr/local/bin/netserver /usr/local/bin/netserver



# Copy the fortio binaries and files
COPY --from=fortiobuild /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=fortiobuild /usr/share/fortio/static /usr/share/fortio/static
COPY --from=fortiobuild /usr/share/fortio/templates /usr/share/fortio/templates
COPY --from=fortiobuild /usr/bin/fortio /usr/bin/fortio

COPY nptests /usr/bin/

ENTRYPOINT ["nptests"]
