FROM cloudfoundry/cf-deployment-concourse-tasks
MAINTAINER https://github.com/cloudfoundry/bosh-bootloader

# Create directory for GOPATH
RUN mkdir -p /go/bin

ENV GOPATH /go

# add go and GOPATH/bin to PATH
ENV PATH $GOPATH/bin:/usr/local/go/bin:$PATH

RUN \
      apt-get update && \
      apt-get -qqy install --fix-missing \
            silversearcher-ag \
            tree \
            apt-transport-https \
            openssl \
            unzip \
      && \
      apt-get clean

# Install gcloud
RUN echo "deb https://packages.cloud.google.com/apt cloud-sdk main" | tee -a /etc/apt/sources.list.d/google-cloud-sdk.list && \
  curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key add - && \
  apt-get update && \
  apt-get -qqy install google-cloud-cli

# needed in the task 'create-bosh-source-file'
RUN go install github.com/pivotal-cf/texplate@latest

# add bbl from latest commit
ADD bbl /usr/local/bin/

ADD bosh-bootloader /go/src/github.com/cloudfoundry/bosh-bootloader

RUN cp -r /go/src/github.com/cloudfoundry/bosh-bootloader /var/repos
