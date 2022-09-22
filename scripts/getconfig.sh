#!/usr/bin/env sh

# Instructions:
# Execute `./getconfig.sh`, and follow the instructions displayed on the terminal
# The `*-config.toml` file will be created in the same directory as start.sh
# It is recommended to check the flags generated in config.toml


read -p "* Path to start.sh: " startPath
# check if start.sh is present
if [[ ! -f $startPath ]]
then
    echo "Error: start.sh do not exist."
    exit 1
fi
read -p "* Your Address (press Enter, if not required): " ADD

echo "\nThank you, your inputs are:"
echo "Path to start.sh: "$startPath
echo "Address: "$ADD

confPath=${startPath%.sh}"-config.toml"
echo "Path to the config file: "$confPath
# check if config.toml is present
if [[ -f $confPath ]]
then
    echo "WARN: config.toml exists, data will be overwritten."
fi

# SHA1 hash of `tempStart` -> `3305fe263dd4a999d58f96deb064e21bb70123d9`
sed 's/bor --/go run getconfig.go notYet --/g' $startPath > 3305fe263dd4a999d58f96deb064e21bb70123d9.sh
chmod +x 3305fe263dd4a999d58f96deb064e21bb70123d9.sh
./3305fe263dd4a999d58f96deb064e21bb70123d9.sh $ADD
rm 3305fe263dd4a999d58f96deb064e21bb70123d9.sh


sed -i '' "s%*%'*'%g" ./temp

# read the flags from `./temp`
dumpconfigflags=$(head -1 ./temp)

# run the dumpconfig command with the flags from `./temp`
command="bor dumpconfig "$dumpconfigflags" > "$confPath
bash -c "$command"

rm ./temp

if [[ -f ./tempStaticNodes.json ]]
then
    echo "JSON StaticNodes found"
    staticnodesjson=$(head -1 ./tempStaticNodes.json)
    sed -i '' "s%static-nodes = \[\]%static-nodes = \[\"${staticnodesjson}\"\]%" $confPath
    rm ./tempStaticNodes.json
elif [[ -f ./tempStaticNodes.toml ]]
then
    echo "TOML StaticNodes found"
    staticnodestoml=$(head -1 ./tempStaticNodes.toml)
    sed -i '' "s%static-nodes = \[\]%static-nodes = \[\"${staticnodestoml}\"\]%" $confPath
    rm ./tempStaticNodes.toml
else
    echo "neither JSON nor TOML StaticNodes found"
fi

if [[ -f ./tempTrustedNodes.toml ]]
then
    echo "TOML TrustedNodes found"
    trustednodestoml=$(head -1 ./tempTrustedNodes.toml)
    sed -i '' "s%trusted-nodes = \[\]%trusted-nodes = \[\"${trustednodestoml}\"\]%" $confPath
    rm ./tempTrustedNodes.toml
else
    echo "neither JSON nor TOML TrustedNodes found"
fi

# comment flags in $configPath that were not passed through $startPath
# SHA1 hash of `tempStart` -> `3305fe263dd4a999d58f96deb064e21bb70123d9`
sed "s%bor --%go run getconfig.go ${confPath} --%" $startPath > 3305fe263dd4a999d58f96deb064e21bb70123d9.sh
chmod +x 3305fe263dd4a999d58f96deb064e21bb70123d9.sh
./3305fe263dd4a999d58f96deb064e21bb70123d9.sh $ADD
rm 3305fe263dd4a999d58f96deb064e21bb70123d9.sh

exit 0


# ####################################################
#          <Old Script, just for reference>
# ####################################################

# #!/usr/bin/env sh

# # Instructions:
# # Execute `./getconfig.sh`, and follow the instructions displayed on the terminal
# # The `*-config.toml` file will be created in the same directory as start.sh
# # It is recommended to check the flags generated in config.toml


# read -p "* Path to start.sh: " startPath
# # check if start.sh is present
# if [[ ! -f $startPath ]]
# then
#     echo "Error: start.sh do not exist."
#     exit 1
# fi
# read -p "* Your Address (press Enter, if not required): " ADD
# read -p "* Path to bor directory (default is set to '/var/lib/bor') (no forward slash at the end please!): " BORDIR
# if [[ "$BORDIR" == "" ]]
#   then
#     BORDIR="/var/lib/bor"
# fi
# DATADIR=$BORDIR/data
# echo "\nThank you, your inputs are:"
# echo "Path to start.sh: "$startPath
# echo "Address: "$ADD
# echo "BORDIR: "$BORDIR
# echo "DATADIR: "$DATADIR

# confPath=${startPath%.sh}"-config.toml"
# echo "Path to the config file: "$confPath
# # check if config.toml is present
# if [[ -f $confPath ]]
# then
#     echo "WARN: config.toml exists, data will be overwritten."
# fi

# count=0
# # -n "$line": just prevents the loop from being skipped if the last line 
# #             ends with end-of-file rather than a newline character
# while read line || [ -n "$line" ]; do
#     # check if the current line is the command used to starting bor
#     if [[ $line == bor* ]]; then
#         count=$((count+1))
#         # get the flags by removing everything upto the first `-`
#         flags="-"${line#*-}
#     fi
# done < $startPath

# # check the number of bor commands, exit if not equal to 1
# if [ ! "$count" -eq 1 ]; then
#   echo "ERROR: More than one bor command"
#   exit 1
# fi

# # DONOT DELETE THE BELOW 2 LINES (hint: {137 vs '137'}, {* vs '*'})
# # command="go run getconfig.go "$flags
# # bash -c "$command"
# go run getconfig.go "notYet" $flags

# # read the flags from `./temp`
# dumpconfigflags=$(head -1 ./temp)

# # Removing all occurance of `$`
# tempflags=${dumpconfigflags//$/}

# # using '@' as the path contains '/'
# tempflags=$(echo $tempflags | sed "s@ADDRESS@$ADD@g")
# tempflags=$(echo $tempflags | sed "s@BOR_DIR@$BORDIR@g")
# tempflags=$(echo $tempflags | sed "s@BOR_HOME@$BORDIR@g")
# tempflags=$(echo $tempflags | sed "s@BOR_DATA_DIR@$DATADIR@g")
# tempflags=$(echo $tempflags | sed "s@DATA_DIR@$DATADIR@g")

# # run the dumpconfig command with the flags from `./temp`
# command="bor dumpconfig "$tempflags" > "$confPath
# bash -c "$command"

# rm ./temp

# if [[ -f ./tempStaticNodes.json ]]
# then
#     echo "JSON StaticNodes found"
#     staticnodesjson=$(head -1 ./tempStaticNodes.json)
#     sed -i '' "s%static-nodes = \[\]%static-nodes = \[\"${staticnodesjson}\"\]%" $confPath
#     rm ./tempStaticNodes.json
# elif [[ -f ./tempStaticNodes.toml ]]
# then
#     echo "TOML StaticNodes found"
#     staticnodestoml=$(head -1 ./tempStaticNodes.toml)
#     sed -i '' "s%static-nodes = \[\]%static-nodes = \[\"${staticnodestoml}\"\]%" $confPath
#     rm ./tempStaticNodes.toml
# else
#     echo "neither JSON nor TOML StaticNodes found"
# fi

# if [[ -f ./tempTrustedNodes.toml ]]
# then
#     echo "TOML TrustedNodes found"
#     trustednodestoml=$(head -1 ./tempTrustedNodes.toml)
#     sed -i '' "s%trusted-nodes = \[\]%trusted-nodes = \[\"${trustednodestoml}\"\]%" $confPath
#     rm ./tempTrustedNodes.toml
# else
#     echo "neither JSON nor TOML TrustedNodes found"
# fi

# # comment flags in $configPath that were not passed through $startPath
# go run getconfig.go $confPath $flags

# exit 0