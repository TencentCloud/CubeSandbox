def runWorkers = true
pipeline {
    agent none
    stages {
        stage('Early checks') {
            agent { node { label 'built-in' } }
            stages {
                stage('Checkout') {
                    steps {
                        checkout scm
                    }
                }
                stage('Check for documentation only changes') {
                    when {
                        expression {
                            return docsFileOnly()
                        }
                    }
                    steps {
                        script {
                            runWorkers = false
                            echo 'Documentation only changes, no need to run the CI'
                        }
                    }
                }
                stage('Check for fuzzer files only changes') {
                    when {
                        expression {
                            return fuzzFileOnly()
                        }
                    }
                    steps {
                        script {
                            runWorkers = false
                            echo 'Fuzzer cargo files only changes, no need to run the CI'
                        }
                    }
                }
                stage('Check for RFC/WIP builds') {
                    when {
                        changeRequest comparator: 'REGEXP', title: '.*(rfc|RFC|wip|WIP).*'
                        beforeAgent true
                    }
                    steps {
                        error('Failing as this is marked as a WIP or RFC PR.')
                    }
                }
                stage('Cancel older builds') {
                    when { not { branch 'main' } }
                    steps {
                        cancelPreviousBuilds()
                    }
                }
            }
        }
        stage('Build') {
            parallel {
                stage('Worker build') {
                    agent { node { label 'jammy' } }
                    when {
                        beforeAgent true
                        expression {
                            return runWorkers
                        }
                    }
                    stages {
                        stage('Checkout') {
                            steps {
                                checkout scm
                            }
                        }
                        stage('Prepare environment') {
                            steps {
                                sh 'scripts/prepare_vdpa.sh'
                            }
                        }
                        stage('Run OpenAPI tests') {
                            steps {
                                sh 'scripts/run_openapi_tests.sh'
                            }
                        }
                        stage('Run unit tests') {
                            steps {
                                sh 'scripts/dev_cli.sh tests --unit'
                            }
                        }
                        stage('Run integration tests') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'sudo modprobe openvswitch'
                                sh 'scripts/dev_cli.sh tests --integration'
                            }
                        }
                        stage('Run live-migration integration tests') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'sudo modprobe openvswitch'
                                sh 'scripts/dev_cli.sh tests --integration-live-migration'
                            }
                        }
                        stage('Run unit tests for musl') {
                            steps {
                                sh 'scripts/dev_cli.sh tests --unit --libc musl'
                            }
                        }
                        stage('Run integration tests for musl') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'sudo modprobe openvswitch'
                                sh 'scripts/dev_cli.sh tests --integration --libc musl'
                            }
                        }
                        stage('Run live-migration integration tests for musl') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'sudo modprobe openvswitch'
                                sh 'scripts/dev_cli.sh tests --integration-live-migration --libc musl'
                            }
                        }
                    }
                }
                stage('AArch64 worker build') {
                    agent { node { label 'bionic-arm64' } }
                    when {
                        beforeAgent true
                        expression {
                            return runWorkers
                        }
                    }
                    environment {
                            AZURE_CONNECTION_STRING = credentials('46b4e7d6-315f-4cc1-8333-b58780863b9b')
                    }
                    stages {
                        stage('Checkout') {
                            steps {
                                checkout scm
                            }
                        }
                        stage('Run unit tests') {
                            steps {
                                sh 'scripts/dev_cli.sh tests --unit --libc musl'
                            }
                        }
                        stage('Run integration tests') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'sudo modprobe openvswitch'
                                sh 'scripts/dev_cli.sh tests --integration --libc musl'
                            }
                        }
                        stage('Install azure-cli') {
                            steps {
                                installAzureCli('bionic', 'arm64')
                            }
                        }
                        stage('Download Windows image') {
                            steps {
                                sh '''#!/bin/bash -x
                                    IMG_BASENAME=windows-11-iot-enterprise-aarch64.raw
                                    IMG_PATH=$HOME/workloads/$IMG_BASENAME
                                    IMG_GZ_PATH=$HOME/workloads/$IMG_BASENAME.gz
                                    IMG_GZ_BLOB_NAME=windows-11-iot-enterprise-aarch64-9-min.raw.gz
                                    cp "scripts/$IMG_BASENAME.sha1" "$HOME/workloads/"
                                    pushd "$HOME/workloads"
                                    if sha1sum "$IMG_BASENAME.sha1" --check; then
                                        exit
                                    fi
                                    popd
                                    mkdir -p "$HOME/workloads"
                                    az storage blob download \
                                        --container-name private-images \
                                        --file "$IMG_GZ_PATH" \
                                        --name "$IMG_GZ_BLOB_NAME" \
                                        --connection-string "$AZURE_CONNECTION_STRING"
                                    gzip -d $IMG_GZ_PATH
                                '''
                            }
                        }
                        stage('Run Windows guest integration tests') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'scripts/dev_cli.sh tests --integration-windows --libc musl'
                            }
                        }
                    }
                    post {
                        always {
                            sh "sudo chown -R jenkins.jenkins ${WORKSPACE}"
                            deleteDir()
                        }
                    }
                }
                stage('Worker build - Windows guest') {
                    agent { node { label 'jammy' } }
                    when {
                        beforeAgent true
                        expression {
                            return runWorkers
                        }
                    }
                    environment {
                            AZURE_CONNECTION_STRING = credentials('46b4e7d6-315f-4cc1-8333-b58780863b9b')
                    }
                    stages {
                        stage('Checkout') {
                            steps {
                                checkout scm
                            }
                        }
                        stage('Install azure-cli') {
                            steps {
                                installAzureCli('jammy', 'amd64')
                            }
                        }
                        stage('Download assets') {
                            steps {
                                sh "mkdir ${env.HOME}/workloads"
                                sh 'az storage blob download --container-name private-images --file "$HOME/workloads/windows-server-2019.raw" --name windows-server-2019.raw --connection-string "$AZURE_CONNECTION_STRING"'
                            }
                        }
                        stage('Run Windows guest integration tests') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'scripts/dev_cli.sh tests --integration-windows'
                            }
                        }
                        stage('Run Windows guest integration tests for musl') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'scripts/dev_cli.sh tests --integration-windows --libc musl'
                            }
                        }
                    }
                }
                stage('Worker build - Metrics') {
                    agent { node { label 'jammy-metrics' } }
                    when {
                        branch 'main'
                        beforeAgent true
                        expression {
                            return runWorkers
                        }
                    }
                    environment {
                        METRICS_PUBLISH_KEY = credentials('52e0945f-ce7a-43d1-87af-67d1d87cc40f')
                    }
                    stages {
                        stage('Checkout') {
                            steps {
                                checkout scm
                            }
                        }
                        stage('Run metrics tests') {
                            options {
                                timeout(time: 1, unit: 'HOURS')
                            }
                            steps {
                                sh 'scripts/dev_cli.sh tests --metrics -- -- --report-file /root/workloads/metrics.json'
                            }
                        }
                        stage('Upload metrics report') {
                            steps {
                                sh 'curl -X PUT https://cloud-hypervisor-metrics.azurewebsites.net/api/publishmetrics -H "x-functions-key: $METRICS_PUBLISH_KEY" -T ~/workloads/metrics.json'
                            }
                        }
                    }
                }
            }
        }
    }
    post {
        regression {
            script {
                if (env.BRANCH_NAME == 'main') {
                    slackSend(color: '#ff0000', message: '"main" branch build is now failing', channel: '#jenkins-ci')
                }
            }
        }
        fixed {
            script {
                if (env.BRANCH_NAME == 'main') {
                    slackSend(color: '#00ff00', message: '"main" branch build is now fixed', channel: '#jenkins-ci')
                }
            }
        }
    }
}

def cancelPreviousBuilds() {
    // Check for other instances of this particular build, cancel any that are older than the current one
    def jobName = env.JOB_NAME
    def currentBuildNumber = env.BUILD_NUMBER.toInteger()
    def currentJob = Jenkins.instance.getItemByFullName(jobName)

    // Loop through all instances of this particular job/branch
    for (def build : currentJob.builds) {
        if (build.isBuilding() && (build.number.toInteger() < currentBuildNumber)) {
            echo "Older build still queued. Sending kill signal to build number: ${build.number}"
            build.doStop()
        }
    }
}

def installAzureCli(distro, arch) {
    sh 'sudo apt install -y ca-certificates curl apt-transport-https lsb-release gnupg'
    sh 'curl -sL https://packages.microsoft.com/keys/microsoft.asc | gpg --dearmor | sudo tee /etc/apt/trusted.gpg.d/microsoft.gpg > /dev/null'
    sh "echo \"deb [arch=${arch}] https://packages.microsoft.com/repos/azure-cli/ ${distro} main\" | sudo tee /etc/apt/sources.list.d/azure-cli.list"
    sh 'sudo apt update'
    sh 'sudo apt install -y azure-cli'
}

def boolean docsFileOnly() {
    if (env.CHANGE_TARGET == null) {
        return false
    }

    return sh(
        returnStatus: true,
        script: "git diff --name-only origin/${env.CHANGE_TARGET}... | grep -v '\\.md'"
    ) != 0
}

def boolean fuzzFileOnly() {
    if (env.CHANGE_TARGET == null) {
        return false
    }

    return sh(
        returnStatus: true,
        script: "git diff --name-only origin/${env.CHANGE_TARGET}... | grep -v -E 'fuzz/'"
    ) != 0
}
