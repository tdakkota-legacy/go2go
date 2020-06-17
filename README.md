# Deprecated!
go2go officially [released](https://go.googlesource.com/go/+/refs/heads/dev.go2go/README.go2go.md)!

# go2go 
This project is fork of [Go2 Contract Draft](https://go.googlesource.com/proposal/+/master/design/go2draft-contracts.md) 
implementation written by Robert Griesemer.

Original code can be found [here](https://go-review.googlesource.com/c/go/+/187317).

# Install
  
```
go get -u github.com/tdakkota/go2go/cmd/go2go
```

# Usage
To generate Go1-compatible code run
```
go2go translate stack.go2
```


Usage:

```
Usage: go2go <command> [arguments]

The commands are:

        build      translate and build packages
        run        translate and run list of files
        test       translate and test packages
        translate  translate .go2 files into .go files

```