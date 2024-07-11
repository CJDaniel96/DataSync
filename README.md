# Data Sync Service

## Overview

This project is a data synchronization service that uses SFTP to synchronize files between a local directory and a remote directory. The synchronization can be configured to either pull files from the remote directory to the local directory or push files from the local directory to the remote directory.

## Configuration

The service is configured using a JSON configuration file. The configuration file should contain an array of configurations, each specifying the details for one synchronization task.

### Configuration Fields

- `sshHost`: The hostname or IP address of the SSH server.
- `sshPort`: The port number of the SSH server.
- `user`: The username for SSH authentication.
- `password`: The password for SSH authentication.
- `localDir`: The local directory to synchronize.
- `remoteDir`: The remote directory to synchronize.
- `cron`: The cron expression that defines the schedule for synchronization.
- `action`: The synchronization action, either `pull` or `push`.

### Example Configuration

```json
[
    {
        "sshHost": "example.com",
        "sshPort": 22,
        "user": "username",
        "password": "password",
        "localDir": "/path/to/local/dir",
        "remoteDir": "/path/to/remote/dir",
        "cron": "0 * * * *",
        "action": "pull"
    }
]
```

## Usage

1. *Install Dependencies*: Ensure you have the required dependencies installed. You can use go get to install them.

```sh
go get github.com/kardianos/service
go get github.com/pkg/sftp
go get github.com/robfig/cron/v3
go get golang.org/x/crypto/ssh
```

2. *Build the Project*: Compile the Go code.

```sh
go build -o data_sync
```

3. *Run the Service*: Execute the compiled binary with the configuration file.

```sh
./data_sync -config=config.json
```

## Code Structure

+ *main.go*: The main entry point of the application.
+ *data_sync.go*: Contains the core logic for data synchronization.
+ *config.json*: The configuration file for the service.

## Functions

*syncData*
Synchronizes data between the local and remote directories based on the specified action (pull or push).

*pullData*
Pulls data from the remote directory to the local directory.

*pushData*
Pushes data from the local directory to the remote directory.

*downloadFile*
Downloads a file from the remote directory to the local directory.

*uploadFile*
Uploads a file from the local directory to the remote directory.

## License
This project is licensed under the MIT License.

