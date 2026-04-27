#!/bin/bash

set -e

PROJECT=${1:-switch}

mkdir -p $PROJECT/{tui/{conversation,minds,input,statusbar},msgs,minds,memory,api,config}

touch $PROJECT/main.go

touch $PROJECT/tui/model.go
touch $PROJECT/tui/update.go
touch $PROJECT/tui/view.go

touch $PROJECT/tui/conversation/model.go
touch $PROJECT/tui/conversation/update.go
touch $PROJECT/tui/conversation/view.go

touch $PROJECT/tui/minds/model.go
touch $PROJECT/tui/minds/update.go
touch $PROJECT/tui/minds/view.go

touch $PROJECT/tui/input/model.go
touch $PROJECT/tui/input/update.go
touch $PROJECT/tui/input/view.go

touch $PROJECT/tui/statusbar/model.go
touch $PROJECT/tui/statusbar/update.go
touch $PROJECT/tui/statusbar/view.go

touch $PROJECT/msgs/msgs.go

touch $PROJECT/minds/orchestrator.go
touch $PROJECT/minds/mind.go
touch $PROJECT/minds/deliberation.go
touch $PROJECT/minds/budget.go

touch $PROJECT/memory/store.go
touch $PROJECT/memory/search.go
touch $PROJECT/memory/embed.go

touch $PROJECT/api/client.go
touch $PROJECT/api/anthropic.go
touch $PROJECT/api/openai.go

touch $PROJECT/config/config.go
touch $PROJECT/config/defaults.go

echo "created $PROJECT/"
find $PROJECT -type f | sort
