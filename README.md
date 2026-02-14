# iotop-w
iotop like command line application for Windows, written in Go. 

## Features

- Shows top individual processes consuming disk i/o (adjustable)
- "btop" style appearance using braile characters for graphs
- Disk Pressure showing how much queue is on the system of processes awaiting
access slots to the storage
- Written in golang. App is self contained with no dependencies.
- Open Source MIT licence. 

![iotop-w example](iotop-w.jpg)


## Commands

Use + and - keys to change the rate of display. Minimum is 100ms.  
There are some command line options:  

```
Usage: iotop-w [options]                                                                                                                                                                            Options:                                                                                            --help, -h       Show this help message                                                           --version, -v    Show version                                                                     --info, -i       Show repo info and license                                                       --top <number>   Show top <number> processes (max 20)
```

## Explaination

I made this because there isn't anything else like ths for Windows command line.  
There are plenty of process and memory "top" tools like btm but none of those
look at disk access as a potential bottleneck. On Linux there's iotop and iotop-c etc.
Because of how Windows provides access to the storage, a straight port of iotop isn't
at all straightforward.  


Is it "vibe coded"? Yes. Whilst I can code a bit for Sysadmin needs, I'm not a professional programmer. 
I have unashamedly used some ollama LLM to help me. It would have taken me months to make this otherwise. 


## Roadmap

Features that I'd like to include eventually:

- The terminal process itself can be intrusive, might be nice to have option to exclude it.


## Security

- No network access
- No registry writes
- No file writes
- No elevation required (some processes inaccessible without elevation)

